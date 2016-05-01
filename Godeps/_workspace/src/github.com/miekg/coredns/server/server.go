// Package server implements a configurable, general-purpose web server.
// It relies on configurations obtained from the adjacent config package
// and can execute middleware as defined by the adjacent middleware package.
package server

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/miekg/coredns/middleware"
	"github.com/miekg/coredns/middleware/chaos"
	"github.com/miekg/coredns/middleware/metrics"

	"github.com/miekg/dns"
	"golang.org/x/net/context"
)

// Server represents an instance of a server, which serves
// DNS requests at a particular address (host and port). A
// server is capable of serving numerous zones on
// the same address and the listener may be stopped for
// graceful termination (POSIX only).
type Server struct {
	Addr   string // Address we listen on
	mux    *dns.ServeMux
	server [2]*dns.Server // by convention 0 is tcp and 1 is udp

	tcp        net.Listener
	udp        net.PacketConn
	listenerMu sync.Mutex // protects listener and packetconn

	tls         bool // whether this server is serving all HTTPS hosts or not
	TLSConfig   *tls.Config
	OnDemandTLS bool             // whether this server supports on-demand TLS (load certs at handshake-time)
	zones       map[string]zone  // zones keyed by their address
	dnsWg       sync.WaitGroup   // used to wait on outstanding connections
	startChan   chan struct{}    // used to block until server is finished starting
	connTimeout time.Duration    // the maximum duration of a graceful shutdown
	ReqCallback OptionalCallback // if non-nil, is executed at the beginning of every request
	SNICallback func(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error)
}

// OptionalCallback is a function that may or may not handle a request.
// It returns whether or not it handled the request. If it handled the
// request, it is presumed that no further request handling should occur.
type OptionalCallback func(dns.ResponseWriter, *dns.Msg) bool

// New creates a new Server which will bind to addr and serve
// the sites/hosts configured in configs. Its listener will
// gracefully close when the server is stopped which will take
// no longer than gracefulTimeout.
//
// This function does not start serving.
//
// Do not re-use a server (start, stop, then start again). We
// could probably add more locking to make this possible, but
// as it stands, you should dispose of a server after stopping it.
// The behavior of serving with a spent server is undefined.
func New(addr string, configs []Config, gracefulTimeout time.Duration) (*Server, error) {
	var useTLS, useOnDemandTLS bool
	if len(configs) > 0 {
		useTLS = configs[0].TLS.Enabled
		useOnDemandTLS = configs[0].TLS.OnDemand
	}

	s := &Server{
		Addr:      addr,
		TLSConfig: new(tls.Config),
		// TODO: Make these values configurable?
		// ReadTimeout:    2 * time.Minute,
		// WriteTimeout:   2 * time.Minute,
		// MaxHeaderBytes: 1 << 16,
		tls:         useTLS,
		OnDemandTLS: useOnDemandTLS,
		zones:       make(map[string]zone),
		startChan:   make(chan struct{}),
		connTimeout: gracefulTimeout,
	}
	mux := dns.NewServeMux()
	mux.Handle(".", s) // wildcard handler, everything will go through here
	s.mux = mux

	// We have to bound our wg with one increment
	// to prevent a "race condition" that is hard-coded
	// into sync.WaitGroup.Wait() - basically, an add
	// with a positive delta must be guaranteed to
	// occur before Wait() is called on the wg.
	// In a way, this kind of acts as a safety barrier.
	s.dnsWg.Add(1)

	// Set up each zone
	for _, conf := range configs {
		if _, exists := s.zones[conf.Host]; exists {
			return nil, fmt.Errorf("cannot serve %s - host already defined for address %s", conf.Address(), s.Addr)
		}

		z := zone{config: conf}

		// Build middleware stack
		err := z.buildStack()
		if err != nil {
			return nil, err
		}

		s.zones[conf.Host] = z

		// A bit of a hack. Loop through the middlewares of this zone and check if
		// they have enabled the chaos middleware. If so add the special chaos zones.
	Middleware:
		for _, mid := range z.config.Middleware {
			fn := mid(nil)
			if _, ok := fn.(chaos.Chaos); ok {
				for _, ch := range []string{"authors.bind.", "version.bind.", "version.server.", "hostname.bind.", "id.server."} {
					s.zones[ch] = z
				}
				break Middleware
			}
		}
	}

	return s, nil
}

// LocalAddr return the addresses where the server is bound to. The TCP listener
// address is the first returned, the UDP conn address the second.
func (s *Server) LocalAddr() (net.Addr, net.Addr) {
	s.listenerMu.Lock()
	tcp := s.tcp.Addr()
	udp := s.udp.LocalAddr()
	s.listenerMu.Unlock()
	return tcp, udp
}

// Serve starts the server with an existing listener. It blocks until the server stops.
func (s *Server) Serve(ln net.Listener, pc net.PacketConn) error {
	err := s.setup()
	if err != nil {
		close(s.startChan) // MUST defer so error is properly reported, same with all cases in this file
		return err
	}
	s.listenerMu.Lock()
	s.server[0] = &dns.Server{Listener: ln, Net: "tcp", Handler: s.mux}
	s.tcp = ln
	s.server[1] = &dns.Server{PacketConn: pc, Net: "udp", Handler: s.mux}
	s.udp = pc
	s.listenerMu.Unlock()

	go func() {
		s.server[0].ActivateAndServe()
	}()
	close(s.startChan)
	return s.server[1].ActivateAndServe()
}

// ListenAndServe starts the server with a new listener. It blocks until the server stops.
func (s *Server) ListenAndServe() error {
	err := s.setup()
	// defer close(s.startChan) // Don't understand why defer wouldn't actually work in this method (prolly cause the last ActivateAndServe does not actually return?
	if err != nil {
		close(s.startChan)
		return err
	}

	l, err := net.Listen("tcp", s.Addr)
	if err != nil {
		close(s.startChan)
		return err
	}
	pc, err := net.ListenPacket("udp", s.Addr)
	if err != nil {
		close(s.startChan)
		return err
	}

	s.listenerMu.Lock()
	s.server[0] = &dns.Server{Listener: l, Net: "tcp", Handler: s.mux}
	s.tcp = l
	s.server[1] = &dns.Server{PacketConn: pc, Net: "udp", Handler: s.mux}
	s.udp = pc
	s.listenerMu.Unlock()

	go func() {
		s.server[0].ActivateAndServe()
	}()
	close(s.startChan)
	return s.server[1].ActivateAndServe()
}

// setup prepares the server s to begin listening; it should be
// called just before the listener announces itself on the network
// and should only be called when the server is just starting up.
func (s *Server) setup() error {
	// Execute startup functions now
	for _, z := range s.zones {
		for _, startupFunc := range z.config.Startup {
			err := startupFunc()
			if err != nil {
				return err
			}
		}
	}

	return nil
}

/*
TODO(miek): no such thing in the glorious Go DNS.
// serveTLS serves TLS with SNI and client auth support if s has them enabled. It
// blocks until s quits.
func serveTLS(s *Server, ln net.Listener, tlsConfigs []TLSConfig) error {
	// Customize our TLS configuration
	s.TLSConfig.MinVersion = tlsConfigs[0].ProtocolMinVersion
	s.TLSConfig.MaxVersion = tlsConfigs[0].ProtocolMaxVersion
	s.TLSConfig.CipherSuites = tlsConfigs[0].Ciphers
	s.TLSConfig.PreferServerCipherSuites = tlsConfigs[0].PreferServerCipherSuites

	// TLS client authentication, if user enabled it
	err := setupClientAuth(tlsConfigs, s.TLSConfig)
	if err != nil {
		defer close(s.startChan)
		return err
	}

	// Create TLS listener - note that we do not replace s.listener
	// with this TLS listener; tls.listener is unexported and does
	// not implement the File() method we need for graceful restarts
	// on POSIX systems.
	ln = tls.NewListener(ln, s.TLSConfig)

	close(s.startChan) // unblock anyone waiting for this to start listening
	return s.Serve(ln)
}
*/

// Stop stops the server. It blocks until the server is
// totally stopped. On POSIX systems, it will wait for
// connections to close (up to a max timeout of a few
// seconds); on Windows it will close the listener
// immediately.
func (s *Server) Stop() (err error) {

	if runtime.GOOS != "windows" {
		// force connections to close after timeout
		done := make(chan struct{})
		go func() {
			s.dnsWg.Done() // decrement our initial increment used as a barrier
			s.dnsWg.Wait()
			close(done)
		}()

		// Wait for remaining connections to finish or
		// force them all to close after timeout
		select {
		case <-time.After(s.connTimeout):
		case <-done:
		}
	}

	// Close the listener now; this stops the server without delay
	s.listenerMu.Lock()
	if s.tcp != nil {
		err = s.tcp.Close()
	}
	if s.udp != nil {
		err = s.udp.Close()
	}

	for _, s1 := range s.server {
		err = s1.Shutdown()
	}
	s.listenerMu.Unlock()

	return
}

// WaitUntilStarted blocks until the server s is started, meaning
// that practically the next instruction is to start the server loop.
// It also unblocks if the server encounters an error during startup.
func (s *Server) WaitUntilStarted() {
	<-s.startChan
}

// ListenerFd gets a dup'ed file of the listener. If there
// is no underlying file, the return value will be nil. It
// is the caller's responsibility to close the file.
func (s *Server) ListenerFd() *os.File {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.tcp != nil {
		file, _ := s.tcp.(*net.TCPListener).File()
		return file
	}
	return nil
}

// PacketConnFd gets a dup'ed file of the packetconn. If there
// is no underlying file, the return value will be nil. It
// is the caller's responsibility to close the file.
func (s *Server) PacketConnFd() *os.File {
	s.listenerMu.Lock()
	defer s.listenerMu.Unlock()
	if s.udp != nil {
		file, _ := s.udp.(*net.UDPConn).File()
		return file
	}
	return nil
}

// ServeDNS is the entry point for every request to the address that s
// is bound to. It acts as a multiplexer for the requests zonename as
// defined in the request so that the correct zone
// (configuration and middleware stack) will handle the request.
func (s *Server) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	defer func() {
		// In case the user doesn't enable error middleware, we still
		// need to make sure that we stay alive up here
		if rec := recover(); rec != nil {
			DefaultErrorFunc(w, r, dns.RcodeServerFailure)
		}
	}()

	if m, err := middleware.Edns0Version(r); err != nil { // Wrong EDNS version, return at once.
		rc := middleware.RcodeToString(dns.RcodeBadVers)
		// TODO(miek): hardcoded "udp" here.
		metrics.Report(metrics.Dropped, "udp", rc, m.Len(), time.Now())
		w.WriteMsg(m)
		return
	}

	// Execute the optional request callback if it exists
	if s.ReqCallback != nil && s.ReqCallback(w, r) {
		return
	}

	q := r.Question[0].Name
	b := make([]byte, len(q))
	off, end := 0, false
	ctx := context.Background()

	for {
		l := len(q[off:])
		for i := 0; i < l; i++ {
			b[i] = q[off+i]
			// normalize the name for the lookup
			if b[i] >= 'A' && b[i] <= 'Z' {
				b[i] |= ('a' - 'A')
			}
		}

		if h, ok := s.zones[string(b[:l])]; ok {
			if r.Question[0].Qtype != dns.TypeDS {
				rcode, _ := h.stack.ServeDNS(ctx, w, r)
				if RcodeNoClientWrite(rcode) {
					DefaultErrorFunc(w, r, rcode)
				}
				return
			}
		}
		off, end = dns.NextLabel(q, off)
		if end {
			break
		}
	}
	// Wildcard match, if we have found nothing try the root zone as a last resort.
	if h, ok := s.zones["."]; ok {
		rcode, _ := h.stack.ServeDNS(ctx, w, r)
		if RcodeNoClientWrite(rcode) {
			DefaultErrorFunc(w, r, rcode)
		}
		return
	}

	// Still here? Error out with REFUSED and some logging
	remoteHost := w.RemoteAddr().String()
	DefaultErrorFunc(w, r, dns.RcodeRefused)
	log.Printf("[INFO] \"%s %s %s\" - No such zone at %s (Remote: %s)", dns.Type(r.Question[0].Qtype), dns.Class(r.Question[0].Qclass), q, s.Addr, remoteHost)
}

// DefaultErrorFunc responds to an DNS request with an error.
func DefaultErrorFunc(w dns.ResponseWriter, r *dns.Msg, rcode int) {
	state := middleware.State{W: w, Req: r}
	rc := middleware.RcodeToString(rcode)

	answer := new(dns.Msg)
	answer.SetRcode(r, rcode)
	state.SizeAndDo(answer)

	metrics.Report(metrics.Dropped, state.Proto(), rc, answer.Len(), time.Now())
	w.WriteMsg(answer)
}

// setupClientAuth sets up TLS client authentication only if
// any of the TLS configs specified at least one cert file.
func setupClientAuth(tlsConfigs []TLSConfig, config *tls.Config) error {
	var clientAuth bool
	for _, cfg := range tlsConfigs {
		if len(cfg.ClientCerts) > 0 {
			clientAuth = true
			break
		}
	}

	if clientAuth {
		pool := x509.NewCertPool()
		for _, cfg := range tlsConfigs {
			for _, caFile := range cfg.ClientCerts {
				caCrt, err := ioutil.ReadFile(caFile) // Anyone that gets a cert from this CA can connect
				if err != nil {
					return err
				}
				if !pool.AppendCertsFromPEM(caCrt) {
					return fmt.Errorf("error loading client certificate '%s': no certificates were successfully parsed", caFile)
				}
			}
		}
		config.ClientCAs = pool
		config.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return nil
}

// RunFirstStartupFuncs runs all of the server's FirstStartup
// callback functions unless one of them returns an error first.
// It is the caller's responsibility to call this only once and
// at the correct time. The functions here should not be executed
// at restarts or where the user does not explicitly start a new
// instance of the server.
func (s *Server) RunFirstStartupFuncs() error {
	for _, z := range s.zones {
		for _, f := range z.config.FirstStartup {
			if err := f(); err != nil {
				return err
			}
		}
	}
	return nil
}

// ShutdownCallbacks executes all the shutdown callbacks
// for all the virtualhosts in servers, and returns all the
// errors generated during their execution. In other words,
// an error executing one shutdown callback does not stop
// execution of others. Only one shutdown callback is executed
// at a time. You must protect the servers that are passed in
// if they are shared across threads.
func ShutdownCallbacks(servers []*Server) []error {
	var errs []error
	for _, s := range servers {
		for _, zone := range s.zones {
			for _, shutdownFunc := range zone.config.Shutdown {
				err := shutdownFunc()
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errs
}

func StartupCallbacks(servers []*Server) []error {
	var errs []error
	for _, s := range servers {
		for _, zone := range s.zones {
			for _, startupFunc := range zone.config.Startup {
				err := startupFunc()
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errs
}

func RcodeNoClientWrite(rcode int) bool {
	switch rcode {
	case dns.RcodeServerFailure:
		fallthrough
	case dns.RcodeRefused:
		fallthrough
	case dns.RcodeFormatError:
		fallthrough
	case dns.RcodeNotImplemented:
		return true
	}
	return false
}
