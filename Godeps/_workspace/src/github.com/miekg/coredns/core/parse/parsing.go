package parse

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/miekg/dns"
)

type parser struct {
	Dispenser
	block           ServerBlock // current server block being parsed
	eof             bool        // if we encounter a valid EOF in a hard place
	checkDirectives bool        // if true, directives must be known
}

func (p *parser) parseAll() ([]ServerBlock, error) {
	var blocks []ServerBlock

	for p.Next() {
		err := p.parseOne()
		if err != nil {
			return blocks, err
		}
		if len(p.block.Addresses) > 0 {
			blocks = append(blocks, p.block)
		}
	}

	return blocks, nil
}

func (p *parser) parseOne() error {
	p.block = ServerBlock{Tokens: make(map[string][]token)}

	err := p.begin()
	if err != nil {
		return err
	}

	return nil
}

func (p *parser) begin() error {
	if len(p.tokens) == 0 {
		return nil
	}

	err := p.addresses()
	if err != nil {
		return err
	}

	if p.eof {
		// this happens if the Corefile consists of only
		// a line of addresses and nothing else
		return nil
	}

	err = p.blockContents()
	if err != nil {
		return err
	}

	return nil
}

func (p *parser) addresses() error {
	var expectingAnother bool

	for {
		tkn := replaceEnvVars(p.Val())

		// special case: import directive replaces tokens during parse-time
		if tkn == "import" && p.isNewLine() {
			err := p.doImport()
			if err != nil {
				return err
			}
			continue
		}

		// Open brace definitely indicates end of addresses
		if tkn == "{" {
			if expectingAnother {
				return p.Errf("Expected another address but had '%s' - check for extra comma", tkn)
			}
			break
		}

		if tkn != "" { // empty token possible if user typed "" in Corefile
			// Trailing comma indicates another address will follow, which
			// may possibly be on the next line
			if tkn[len(tkn)-1] == ',' {
				tkn = tkn[:len(tkn)-1]
				expectingAnother = true
			} else {
				expectingAnother = false // but we may still see another one on this line
			}

			// Parse and save this address
			addr, err := standardAddress(tkn)
			if err != nil {
				return err
			}
			p.block.Addresses = append(p.block.Addresses, addr)
		}

		// Advance token and possibly break out of loop or return error
		hasNext := p.Next()
		if expectingAnother && !hasNext {
			return p.EOFErr()
		}
		if !hasNext {
			p.eof = true
			break // EOF
		}
		if !expectingAnother && p.isNewLine() {
			break
		}
	}

	return nil
}

func (p *parser) blockContents() error {
	errOpenCurlyBrace := p.openCurlyBrace()
	if errOpenCurlyBrace != nil {
		// single-server configs don't need curly braces
		p.cursor--
	}

	err := p.directives()
	if err != nil {
		return err
	}

	// Only look for close curly brace if there was an opening
	if errOpenCurlyBrace == nil {
		err = p.closeCurlyBrace()
		if err != nil {
			return err
		}
	}

	return nil
}

// directives parses through all the lines for directives
// and it expects the next token to be the first
// directive. It goes until EOF or closing curly brace
// which ends the server block.
func (p *parser) directives() error {
	for p.Next() {
		// end of server block
		if p.Val() == "}" {
			break
		}

		// special case: import directive replaces tokens during parse-time
		if p.Val() == "import" {
			err := p.doImport()
			if err != nil {
				return err
			}
			p.cursor-- // cursor is advanced when we continue, so roll back one more
			continue
		}

		// normal case: parse a directive on this line
		if err := p.directive(); err != nil {
			return err
		}
	}
	return nil
}

// doImport swaps out the import directive and its argument
// (a total of 2 tokens) with the tokens in the specified file
// or globbing pattern. When the function returns, the cursor
// is on the token before where the import directive was. In
// other words, call Next() to access the first token that was
// imported.
func (p *parser) doImport() error {
	// syntax check
	if !p.NextArg() {
		return p.ArgErr()
	}
	importPattern := p.Val()
	if p.NextArg() {
		return p.Err("Import takes only one argument (glob pattern or file)")
	}

	// do glob
	matches, err := filepath.Glob(importPattern)
	if err != nil {
		return p.Errf("Failed to use import pattern %s: %v", importPattern, err)
	}
	if len(matches) == 0 {
		return p.Errf("No files matching import pattern %s", importPattern)
	}

	// splice out the import directive and its argument (2 tokens total)
	tokensBefore := p.tokens[:p.cursor-1]
	tokensAfter := p.tokens[p.cursor+1:]

	// collect all the imported tokens
	var importedTokens []token
	for _, importFile := range matches {
		newTokens, err := p.doSingleImport(importFile)
		if err != nil {
			return err
		}
		importedTokens = append(importedTokens, newTokens...)
	}

	// splice the imported tokens in the place of the import statement
	// and rewind cursor so Next() will land on first imported token
	p.tokens = append(tokensBefore, append(importedTokens, tokensAfter...)...)
	p.cursor--

	return nil
}

// doSingleImport lexes the individual file at importFile and returns
// its tokens or an error, if any.
func (p *parser) doSingleImport(importFile string) ([]token, error) {
	file, err := os.Open(importFile)
	if err != nil {
		return nil, p.Errf("Could not import %s: %v", importFile, err)
	}
	defer file.Close()
	importedTokens := allTokens(file)

	// Tack the filename onto these tokens so errors show the imported file's name
	filename := filepath.Base(importFile)
	for i := 0; i < len(importedTokens); i++ {
		importedTokens[i].file = filename
	}

	return importedTokens, nil
}

// directive collects tokens until the directive's scope
// closes (either end of line or end of curly brace block).
// It expects the currently-loaded token to be a directive
// (or } that ends a server block). The collected tokens
// are loaded into the current server block for later use
// by directive setup functions.
func (p *parser) directive() error {
	dir := p.Val()
	nesting := 0

	if p.checkDirectives {
		if _, ok := ValidDirectives[dir]; !ok {
			return p.Errf("Unknown directive '%s'", dir)
		}
	}

	// The directive itself is appended as a relevant token
	p.block.Tokens[dir] = append(p.block.Tokens[dir], p.tokens[p.cursor])

	for p.Next() {
		if p.Val() == "{" {
			nesting++
		} else if p.isNewLine() && nesting == 0 {
			p.cursor-- // read too far
			break
		} else if p.Val() == "}" && nesting > 0 {
			nesting--
		} else if p.Val() == "}" && nesting == 0 {
			return p.Err("Unexpected '}' because no matching opening brace")
		}
		p.tokens[p.cursor].text = replaceEnvVars(p.tokens[p.cursor].text)
		p.block.Tokens[dir] = append(p.block.Tokens[dir], p.tokens[p.cursor])
	}

	if nesting > 0 {
		return p.EOFErr()
	}
	return nil
}

// openCurlyBrace expects the current token to be an
// opening curly brace. This acts like an assertion
// because it returns an error if the token is not
// a opening curly brace. It does NOT advance the token.
func (p *parser) openCurlyBrace() error {
	if p.Val() != "{" {
		return p.SyntaxErr("{")
	}
	return nil
}

// closeCurlyBrace expects the current token to be
// a closing curly brace. This acts like an assertion
// because it returns an error if the token is not
// a closing curly brace. It does NOT advance the token.
func (p *parser) closeCurlyBrace() error {
	if p.Val() != "}" {
		return p.SyntaxErr("}")
	}
	return nil
}

// standardAddress parses an address string into a structured format with separate
// host, and port portions, as well as the original input string.
func standardAddress(str string) (address, error) {
	var err error

	// first check for scheme and strip it off
	input := str

	// separate host and port
	host, port, err := net.SplitHostPort(str)
	if err != nil {
		host, port, err = net.SplitHostPort(str + ":")
		// no error check here; return err at end of function
	}

	if len(host) > 255 {
		return address{}, fmt.Errorf("specified address is too long: %d > 255", len(host))
	}
	_, d := dns.IsDomainName(host)
	if !d {
		return address{}, fmt.Errorf("host is not a valid domain: %s", host)
	}

	// see if we can set port based off scheme
	if port == "" {
		port = "53"
	}

	return address{Original: input, Host: strings.ToLower(dns.Fqdn(host)), Port: port}, err
}

// replaceEnvVars replaces environment variables that appear in the token
// and understands both the $UNIX and %WINDOWS% syntaxes.
func replaceEnvVars(s string) string {
	s = replaceEnvReferences(s, "{%", "%}")
	s = replaceEnvReferences(s, "{$", "}")
	return s
}

// replaceEnvReferences performs the actual replacement of env variables
// in s, given the placeholder start and placeholder end strings.
func replaceEnvReferences(s, refStart, refEnd string) string {
	index := strings.Index(s, refStart)
	for index != -1 {
		endIndex := strings.Index(s, refEnd)
		if endIndex != -1 {
			ref := s[index : endIndex+len(refEnd)]
			s = strings.Replace(s, ref, os.Getenv(ref[len(refStart):len(ref)-len(refEnd)]), -1)
		} else {
			return s
		}
		index = strings.Index(s, refStart)
	}
	return s
}

type (
	// ServerBlock associates tokens with a list of addresses
	// and groups tokens by directive name.
	ServerBlock struct {
		Addresses []address
		Tokens    map[string][]token
	}

	address struct {
		Original, Host, Port string
	}
)

// HostList converts the list of addresses that are
// associated with this server block into a slice of
// strings, where each address is as it was originally
// read from the input.
func (sb ServerBlock) HostList() []string {
	sbHosts := make([]string, len(sb.Addresses))
	for j, addr := range sb.Addresses {
		sbHosts[j] = addr.Original
	}
	return sbHosts
}
