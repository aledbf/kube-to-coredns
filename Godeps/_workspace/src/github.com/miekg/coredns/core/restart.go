// +build !windows

package core

import (
	"bytes"
	"encoding/gob"
	"errors"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"sync/atomic"

	"github.com/miekg/coredns/core/https"
)

func init() {
	gob.Register(CorefileInput{})
}

// Restart restarts the entire application; gracefully with zero
// downtime if on a POSIX-compatible system, or forcefully if on
// Windows but with imperceptibly-short downtime.
//
// The restarted application will use newCorefile as its input
// configuration. If newCorefile is nil, the current (existing)
// Corefile configuration will be used.
//
// Note: The process must exist in the same place on the disk in
// order for this to work. Thus, multiple graceful restarts don't
// work if executing with `go run`, since the binary is cleaned up
// when `go run` sees the initial parent process exit.
func Restart(newCorefile Input) error {
	log.Println("[INFO] Restarting")

	if newCorefile == nil {
		corefileMu.Lock()
		newCorefile = corefile
		corefileMu.Unlock()
	}

	// Get certificates for any new hosts in the new Corefile without causing downtime
	err := getCertsForNewCorefile(newCorefile)
	if err != nil {
		return errors.New("TLS preload: " + err.Error())
	}

	if len(os.Args) == 0 { // this should never happen, but...
		os.Args = []string{""}
	}

	// Tell the child that it's a restart
	os.Setenv("COREDNS_RESTART", "true")

	// Prepare our payload to the child process
	crfileGob := corefileGob{
		ListenerFds:            make(map[string]uintptr),
		Corefile:               newCorefile,
		OnDemandTLSCertsIssued: atomic.LoadInt32(https.OnDemandIssuedCount),
	}

	// Prepare a pipe to the fork's stdin so it can get the Corefile
	rpipe, wpipe, err := os.Pipe()
	if err != nil {
		return err
	}

	// Prepare a pipe that the child process will use to communicate
	// its success with us by sending > 0 bytes
	sigrpipe, sigwpipe, err := os.Pipe()
	if err != nil {
		return err
	}

	// Pass along relevant file descriptors to child process; ordering
	// is very important since we rely on these being in certain positions.
	extraFiles := []*os.File{sigwpipe} // fd 3

	// Add file descriptors of all the sockets
	serversMu.Lock()
	j := 0
	for _, s := range servers {
		extraFiles = append(extraFiles, s.ListenerFd())
		extraFiles = append(extraFiles, s.PacketConnFd())
		// So this will be 0 1 2 3 TCP UDP TCP UDP ... etc.
		crfileGob.ListenerFds["tcp"+s.Addr] = uintptr(4 + j)     // 4 fds come before any of the listeners
		crfileGob.ListenerFds["udp"+s.Addr] = uintptr(4 + j + 1) // add udp after that
		j += 2
	}
	serversMu.Unlock()

	// Set up the command
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Stdin = rpipe      // fd 0
	cmd.Stdout = os.Stdout // fd 1
	cmd.Stderr = os.Stderr // fd 2
	cmd.ExtraFiles = extraFiles

	// Spawn the child process
	err = cmd.Start()
	if err != nil {
		return err
	}

	// Immediately close our dup'ed fds and the write end of our signal pipe
	for _, f := range extraFiles {
		f.Close()
	}

	// Feed Corefile to the child
	err = gob.NewEncoder(wpipe).Encode(crfileGob)
	if err != nil {
		return err
	}
	wpipe.Close()

	// Run all shutdown functions for the middleware, if child start fails, restart them all...
	executeShutdownCallbacks("SIGUSR1")

	// Determine whether child startup succeeded
	answer, readErr := ioutil.ReadAll(sigrpipe)
	if answer == nil || len(answer) == 0 {
		cmdErr := cmd.Wait() // get exit status
		log.Printf("[ERROR] Restart: child failed to initialize (%v) - changes not applied", cmdErr)
		if readErr != nil {
			log.Printf("[ERROR] Restart: additionally, error communicating with child process: %v", readErr)
		}
		// re-call all startup functions.
		// TODO(miek): this needs to be tested, somehow.
		executeStartupCallbacks("SIGUSR1")
		return errIncompleteRestart
	}

	// Looks like child is successful; we can exit gracefully.
	return Stop()
}

func getCertsForNewCorefile(newCorefile Input) error {
	// parse the new corefile only up to (and including) TLS
	// so we can know what we need to get certs for.
	configs, _, _, err := loadConfigsUpToIncludingTLS(path.Base(newCorefile.Path()), bytes.NewReader(newCorefile.Body()))
	if err != nil {
		return errors.New("loading Corefile: " + err.Error())
	}

	// first mark the configs that are qualified for managed TLS
	https.MarkQualified(configs)

	// since we group by bind address to obtain certs, we must call
	// EnableTLS to make sure the port is set properly first
	// (can ignore error since we aren't actually using the certs)
	https.EnableTLS(configs, false)

	// find out if we can let the acme package start its own challenge listener
	// on port 80
	var proxyACME bool
	serversMu.Lock()
	for _, s := range servers {
		_, port, _ := net.SplitHostPort(s.Addr)
		if port == "80" {
			proxyACME = true
			break
		}
	}
	serversMu.Unlock()

	// place certs on the disk
	err = https.ObtainCerts(configs, false, proxyACME)
	if err != nil {
		return errors.New("obtaining certs: " + err.Error())
	}

	return nil
}
