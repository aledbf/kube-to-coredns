package main

import (
	"github.com/golang/glog"
	"github.com/miekg/coredns/core"
)

type backend interface {
	AddHost(fqdn, ip string) error
	RemoveHost(fqdn, ip string) error

	AddCname(alias, fqdn string) error
	RemoveCname(alias string) error

	AddSrv(fqdn, target string, port int) error
	RemoveSrv(fqdn, target string, port int) error

	Start()
}

var _ backend = coredns{}

type coredns struct {
	cfg core.Input
}

func newDNSServer(domain string) (*backend, error) {
	corefile, err := core.LoadCorefile("/corefile")
	if err != nil {
		return nil, err
	}

	return &coredns{
		cfg: corefile,
	}, nil
}

func (c *coredns) Start() {
	err := core.Start(c.cfg)
	if err != nil {
		glog.Fatalf("unexpected error starting coredns: %v", err)
	}

	core.Wait()
}

func (c *coredns) AddHost(fqdn, ip string) error {
	return nil
}

func (c *coredns) RemoveHost(fqdn, ip string) error {
	return nil
}

func (c *coredns) AddCname(alias, fqdn string) error {
	return nil
}

func (c *coredns) RemoveCname(alias string) error {
	return nil
}

func (c *coredns) AddSrv(fqdn, target string, port int) error {
	return nil
}

func (c *coredns) RemoveSrv(fqdn, target string, port int) error {
	return nil
}
