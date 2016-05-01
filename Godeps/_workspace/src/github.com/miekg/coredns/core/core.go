// Package caddy implements the CoreDNS web server as a service
// in your own Go programs.
//
// To use this package, follow a few simple steps:
//
//   1. Set the AppName and AppVersion variables.
//   2. Call LoadCorefile() to get the Corefile (it
//      might have been piped in as part of a restart).
//      You should pass in your own Corefile loader.
//   3. Call caddy.Start() to start CoreDNS, caddy.Stop()
//      to stop it, or caddy.Restart() to restart it.
//
// You should use caddy.Wait() to wait for all CoreDNS servers
// to quit before your process exits.
package core

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/coredns/core/https"
	"github.com/miekg/coredns/server"
)

// Configurable application parameters
var (
	// AppName is the name of the application.
	AppName string

	// AppVersion is the version of the application.
	AppVersion string

	// Quiet when set to true, will not show any informative output on initialization.
	Quiet bool

	// PidFile is the path to the pidfile to create.
	PidFile string

	// GracefulTimeout is the maximum duration of a graceful shutdown.
	GracefulTimeout time.Duration
)

var (
	// corefile is the input configuration text used for this process
	corefile Input

	// corefileMu protects corefile during changes
	corefileMu sync.Mutex

	// errIncompleteRestart occurs if this process is a fork
	// of the parent but no Corefile was piped in
	errIncompleteRestart = errors.New("incomplete restart")

	// servers is a list of all the currently-listening servers
	servers []*server.Server

	// serversMu protects the servers slice during changes
	serversMu sync.Mutex

	// wg is used to wait for all servers to shut down
	wg sync.WaitGroup

	// loadedGob is used if this is a child process as part of
	// a graceful restart; it is used to map listeners to their
	// index in the list of inherited file descriptors. This
	// variable is not safe for concurrent access.
	loadedGob corefileGob

	// startedBefore should be set to true if CoreDNS has been started
	// at least once (does not indicate whether currently running).
	startedBefore bool
)

const (
	// DefaultHost is the default host.
	DefaultHost = ""
	// DefaultPort is the default port.
	DefaultPort = "53"
	// DefaultRoot is the default root folder.
	DefaultRoot = "."
)

// Start starts CoreDNS with the given Corefile. If crfile
// is nil, the LoadCorefile function will be called to get
// one.
//
// This function blocks until all the servers are listening.
//
// Note (POSIX): If Start is called in the child process of a
// restart more than once within the duration of the graceful
// cutoff (i.e. the child process called Start a first time,
// then called Stop, then Start again within the first 5 seconds
// or however long GracefulTimeout is) and the Corefiles have
// at least one listener address in common, the second Start
// may fail with "address already in use" as there's no
// guarantee that the parent process has relinquished the
// address before the grace period ends.
func Start(crfile Input) (err error) {
	// If we return with no errors, we must do two things: tell the
	// parent that we succeeded and write to the pidfile.
	defer func() {
		if err == nil {
			signalSuccessToParent() // TODO: Is doing this more than once per process a bad idea? Start could get called more than once in other apps.
			if PidFile != "" {
				err := writePidFile()
				if err != nil {
					log.Printf("[ERROR] Could not write pidfile: %v", err)
				}
			}
		}
	}()

	// Input must never be nil; try to load something
	if crfile == nil {
		crfile, err = LoadCorefile(nil)
		if err != nil {
			return err
		}
	}

	corefileMu.Lock()
	corefile = crfile
	corefileMu.Unlock()

	// load the server configs (activates Let's Encrypt)
	configs, err := loadConfigs(path.Base(crfile.Path()), bytes.NewReader(crfile.Body()))
	if err != nil {
		return err
	}

	// group zones by address
	groupings, err := arrangeBindings(configs)
	if err != nil {
		return err
	}

	// Start each server with its one or more configurations
	err = startServers(groupings)
	if err != nil {
		return err
	}
	startedBefore = true

	// Show initialization output
	if !Quiet && !IsRestart() {
		var checkedFdLimit bool
		for _, group := range groupings {
			for _, conf := range group.Configs {
				// Print address of site
				fmt.Println(conf.Address())

				// Note if non-localhost site resolves to loopback interface
				if group.BindAddr.IP.IsLoopback() && !isLocalhost(conf.Host) {
					fmt.Printf("Notice: %s is only accessible on this machine (%s)\n",
						conf.Host, group.BindAddr.IP.String())
				}
				if !checkedFdLimit && !group.BindAddr.IP.IsLoopback() && !isLocalhost(conf.Host) {
					checkFdlimit()
					checkedFdLimit = true
				}
			}
		}
	}

	return nil
}

// startServers starts all the servers in groupings,
// taking into account whether or not this process is
// a child from a graceful restart or not. It blocks
// until the servers are listening.
func startServers(groupings bindingGroup) error {
	var startupWg sync.WaitGroup
	errChan := make(chan error, len(groupings)) // must be buffered to allow Serve functions below to return if stopped later

	for _, group := range groupings {
		s, err := server.New(group.BindAddr.String(), group.Configs, GracefulTimeout)
		if err != nil {
			return err
		}
		// TODO(miek): does not work, because this callback uses http instead of dns
		//		s.ReqCallback = https.RequestCallback // ensures we can solve ACME challenges while running
		if s.OnDemandTLS {
			s.TLSConfig.GetCertificate = https.GetOrObtainCertificate // TLS on demand -- awesome!
		} else {
			s.TLSConfig.GetCertificate = https.GetCertificate
		}

		var (
			ln net.Listener
			pc net.PacketConn
		)

		if IsRestart() {
			// Look up this server's listener in the map of inherited file descriptors; if we don't have one, we must make a new one (later).
			if fdIndex, ok := loadedGob.ListenerFds["tcp"+s.Addr]; ok {
				file := os.NewFile(fdIndex, "")

				fln, err := net.FileListener(file)
				if err != nil {
					return err
				}

				ln, ok = fln.(*net.TCPListener)
				if !ok {
					return errors.New("listener for " + s.Addr + " was not a *net.TCPListener")
				}

				file.Close()
				delete(loadedGob.ListenerFds, "tcp"+s.Addr)
			}
			if fdIndex, ok := loadedGob.ListenerFds["udp"+s.Addr]; ok {
				file := os.NewFile(fdIndex, "")

				fpc, err := net.FilePacketConn(file)
				if err != nil {
					return err
				}

				pc, ok = fpc.(*net.UDPConn)
				if !ok {
					return errors.New("packetConn for " + s.Addr + " was not a *net.PacketConn")
				}

				file.Close()
				delete(loadedGob.ListenerFds, "udp"+s.Addr)
			}
		}

		wg.Add(1)
		go func(s *server.Server, ln net.Listener, pc net.PacketConn) {
			defer wg.Done()

			// run startup functions that should only execute when the original parent process is starting.
			if !IsRestart() && !startedBefore {
				err := s.RunFirstStartupFuncs()
				if err != nil {
					errChan <- err
					return
				}
			}

			// start the server
			if ln != nil && pc != nil {
				errChan <- s.Serve(ln, pc)
			} else {
				errChan <- s.ListenAndServe()
			}
		}(s, ln, pc)

		startupWg.Add(1)
		go func(s *server.Server) {
			defer startupWg.Done()
			s.WaitUntilStarted()
		}(s)

		serversMu.Lock()
		servers = append(servers, s)
		serversMu.Unlock()
	}

	// Close the remaining (unused) file descriptors to free up resources
	if IsRestart() {
		for key, fdIndex := range loadedGob.ListenerFds {
			os.NewFile(fdIndex, "").Close()
			delete(loadedGob.ListenerFds, key)
		}
	}

	// Wait for all servers to finish starting
	startupWg.Wait()

	// Return the first error, if any
	select {
	case err := <-errChan:
		// "use of closed network connection" is normal if it was a graceful shutdown
		if err != nil && !strings.Contains(err.Error(), "use of closed network connection") {
			return err
		}
	default:
	}

	return nil
}

// Stop stops all servers. It blocks until they are all stopped.
// It does NOT execute shutdown callbacks that may have been
// configured by middleware (they must be executed separately).
func Stop() error {
	https.Deactivate()

	serversMu.Lock()
	for _, s := range servers {
		if err := s.Stop(); err != nil {
			log.Printf("[ERROR] Stopping %s: %v", s.Addr, err)
		}
	}
	servers = []*server.Server{} // don't reuse servers
	serversMu.Unlock()

	return nil
}

// Wait blocks until all servers are stopped.
func Wait() {
	wg.Wait()
}

// LoadCorefile loads a Corefile, prioritizing a Corefile
// piped from stdin as part of a restart (only happens on first call
// to LoadCorefile). If it is not a restart, this function tries
// calling the user's loader function, and if that returns nil, then
// this function resorts to the default configuration. Thus, if there
// are no other errors, this function always returns at least the
// default Corefile.
func LoadCorefile(loader func() (Input, error)) (crfile Input, err error) {
	// If we are a fork, finishing the restart is highest priority;
	// piped input is required in this case.
	if IsRestart() {
		err := gob.NewDecoder(os.Stdin).Decode(&loadedGob)
		if err != nil {
			return nil, err
		}
		crfile = loadedGob.Corefile
		atomic.StoreInt32(https.OnDemandIssuedCount, loadedGob.OnDemandTLSCertsIssued)
	}

	// Try user's loader
	if crfile == nil && loader != nil {
		crfile, err = loader()
	}

	// Otherwise revert to default
	if crfile == nil {
		crfile = DefaultInput()
	}

	return
}

// CorefileFromPipe loads the Corefile input from f if f is
// not interactive input. f is assumed to be a pipe or stream,
// such as os.Stdin. If f is not a pipe, no error is returned
// but the Input value will be nil. An error is only returned
// if there was an error reading the pipe, even if the length
// of what was read is 0.
func CorefileFromPipe(f *os.File) (Input, error) {
	fi, err := f.Stat()
	if err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		// Note that a non-nil error is not a problem. Windows
		// will not create a stdin if there is no pipe, which
		// produces an error when calling Stat(). But Unix will
		// make one either way, which is why we also check that
		// bitmask.
		// BUG: Reading from stdin after this fails (e.g. for the let's encrypt email address) (OS X)
		confBody, err := ioutil.ReadAll(f)
		if err != nil {
			return nil, err
		}
		return CorefileInput{
			Contents: confBody,
			Filepath: f.Name(),
		}, nil
	}

	// not having input from the pipe is not itself an error,
	// just means no input to return.
	return nil, nil
}

// Corefile returns the current Corefile
func Corefile() Input {
	corefileMu.Lock()
	defer corefileMu.Unlock()
	return corefile
}

// Input represents a Corefile; its contents and file path
// (which should include the file name at the end of the path).
// If path does not apply (e.g. piped input) you may use
// any understandable value. The path is mainly used for logging,
// error messages, and debugging.
type Input interface {
	// Gets the Corefile contents
	Body() []byte

	// Gets the path to the origin file
	Path() string

	// IsFile returns true if the original input was a file on the file system
	// that could be loaded again later if requested.
	IsFile() bool
}

// TestServer returns a test server.
// The ports can be retreived with server.LocalAddr(). The testserver itself can be stopped
// with Stop(). It just takes a normal Corefile as input.
func TestServer(t *testing.T, corefile string) (*server.Server, error) {

	crfile := CorefileInput{Contents: []byte(corefile)}
	configs, err := loadConfigs(path.Base(crfile.Path()), bytes.NewReader(crfile.Body()))
	if err != nil {
		return nil, err
	}
	groupings, err := arrangeBindings(configs)
	if err != nil {
		return nil, err
	}
	t.Logf("Starting %d servers", len(groupings))

	group := groupings[0]
	s, err := server.New(group.BindAddr.String(), group.Configs, time.Second)
	return s, err
}
