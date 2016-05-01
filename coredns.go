package main

import (
	"io/ioutil"

	"github.com/golang/glog"
	"github.com/miekg/coredns/core"
)

type backend interface {
	AddHost(fqdn, ip string) error
	RemoveHost(fqdn, ip string) error

	AddSrv(fqdn, target string, port int32) error
	RemoveSrv(fqdn, target string, port int32) error

	Start()
}

var _ backend = coredns{}

type coredns struct {
	cfg core.Input
}

func newDNSServer(domain string) (backend, error) {
	contents, err := ioutil.ReadFile("/corefile")
	if err != nil {
		return nil, err
	}

	corefile, err := core.LoadCorefile(func() (core.Input, error) {
		return core.CorefileInput{
			Contents: contents,
			RealFile: false,
		}, nil
	})

	if err != nil {
		return nil, err
	}

	return coredns{
		cfg: corefile,
	}, nil
}

func (c coredns) Start() {
	err := core.Start(c.cfg)
	if err != nil {
		glog.Fatalf("unexpected error starting coredns: %v", err)
	}

	core.Wait()
}

func (c coredns) AddHost(fqdn, ip string) error {
	return nil
}

func (c coredns) RemoveHost(fqdn, ip string) error {
	return nil
}

func (c coredns) AddSrv(fqdn, target string, port int32) error {
	return nil
}

func (c coredns) RemoveSrv(fqdn, target string, port int32) error {
	return nil
}
