package pprof

import (
	"log"
	"net"
	"net/http"
	pp "net/http/pprof"
)

type Handler struct {
	ln  net.Listener
	mux *http.ServeMux
}

func (h *Handler) Start() error {
	if ln, err := net.Listen("tcp", addr); err != nil {
		log.Printf("[ERROR] Failed to start pprof handler: %s", err)
		return err
	} else {
		h.ln = ln
	}

	h.mux = http.NewServeMux()
	h.mux.HandleFunc(path+"/", pp.Index)
	h.mux.HandleFunc(path+"/cmdline", pp.Cmdline)
	h.mux.HandleFunc(path+"/profile", pp.Profile)
	h.mux.HandleFunc(path+"/symbol", pp.Symbol)
	h.mux.HandleFunc(path+"/trace", pp.Trace)

	go func() {
		http.Serve(h.ln, h.mux)
	}()
	return nil
}

func (h *Handler) Shutdown() error {
	if h.ln != nil {
		return h.ln.Close()
	}
	return nil
}

const (
	addr = "localhost:6053"
	path = "/debug/pprof"
)
