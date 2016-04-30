/*
Copyright 2015 The Kubernetes Authors All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
	"k8s.io/kubernetes/pkg/client/cache"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/watch"
)

var (
	keyFunc   = framework.DeletionHandlingMetaNamespaceKeyFunc
	namespace = api.NamespaceAll
)

type dnsController struct {
	client *client.Client

	podController  *framework.Controller
	endpController *framework.Controller
	svcController  *framework.Controller

	podLister  cache.StoreToPodLister
	svcLister  cache.StoreToServiceLister
	endpLister cache.StoreToEndpointsLister

	backend *backend

	// stopLock is used to enforce only a single call to Stop is active.
	// Needed because we allow stopping through an http endpoint and
	// allowing concurrent stoppers leads to stack traces.
	stopLock sync.Mutex
	shutdown bool
	stopCh   chan struct{}
}

// newDNSController creates a controller for coredns
func newdnsController(domain string, kubeClient *client.Client, resyncPeriod time.Duration) (*dnsController, error) {
	dns := dnsController{
		client: kubeClient,
		stopCh: make(chan struct{}),
	}

	bend, err := newDNSServer(domain)
	if err != nil {
		return nil, err
	}

	dns.backend = bend

	podEventHandler := framework.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			dns.handlePodCreate(obj)
		},
		DeleteFunc: func(obj interface{}) {
			dns.handlePodDelete(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				dns.handlePodUpdate(old, cur)
			}
		},
	}

	endpHandler := framework.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			dns.handleEndpointAdd(obj)
		},
		DeleteFunc: func(obj interface{}) {
			//dns.syncQueue.enqueue(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				//dns.syncQueue.enqueue(cur)
			}
		},
	}

	svcHandler := framework.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			dns.handleServiceCreate(obj)
		},
		DeleteFunc: func(obj interface{}) {
			dns.handleServiceRemove(obj)
		},
		UpdateFunc: func(old, cur interface{}) {
			dns.handleServiceUpdate(old, cur)
		},
	}

	dns.podLister.Store, dns.podController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc:  podsListFunc(dns.client, namespace),
			WatchFunc: podsWatchFunc(dns.client, namespace),
		},
		&extensions.Ingress{}, resyncPeriod, podEventHandler)

	dns.endpLister.Store, dns.endpController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc:  endpointsListFunc(dns.client, namespace),
			WatchFunc: endpointsWatchFunc(dns.client, namespace),
		},
		&api.Endpoints{}, resyncPeriod, endpHandler)

	dns.svcLister.Store, dns.svcController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc:  serviceListFunc(dns.client, namespace),
			WatchFunc: serviceWatchFunc(dns.client, namespace),
		},
		&api.Service{}, resyncPeriod, svcHandler)

	return &dns, nil
}

func podsListFunc(c *client.Client, ns string) func(api.ListOptions) (runtime.Object, error) {
	return func(opts api.ListOptions) (runtime.Object, error) {
		return c.Pods(ns).List(opts)
	}
}

func podsWatchFunc(c *client.Client, ns string) func(options api.ListOptions) (watch.Interface, error) {
	return func(options api.ListOptions) (watch.Interface, error) {
		return c.Pods(ns).Watch(options)
	}
}

func serviceListFunc(c *client.Client, ns string) func(api.ListOptions) (runtime.Object, error) {
	return func(opts api.ListOptions) (runtime.Object, error) {
		return c.Services(ns).List(opts)
	}
}

func serviceWatchFunc(c *client.Client, ns string) func(options api.ListOptions) (watch.Interface, error) {
	return func(options api.ListOptions) (watch.Interface, error) {
		return c.Services(ns).Watch(options)
	}
}

func endpointsListFunc(c *client.Client, ns string) func(api.ListOptions) (runtime.Object, error) {
	return func(opts api.ListOptions) (runtime.Object, error) {
		return c.Endpoints(ns).List(opts)
	}
}

func endpointsWatchFunc(c *client.Client, ns string) func(options api.ListOptions) (watch.Interface, error) {
	return func(options api.ListOptions) (watch.Interface, error) {
		return c.Endpoints(ns).Watch(options)
	}
}

func (dns *dnsController) controllersInSync() bool {
	return dns.podController.HasSynced() && dns.svcController.HasSynced() && dns.endpController.HasSynced()
}

// Stop stops the  controller.
func (dns *dnsController) Stop() error {
	// Stop is invoked from the http endpoint.
	dns.stopLock.Lock()
	defer dns.stopLock.Unlock()

	// Only try draining the workqueue if we haven't already.
	if !dns.shutdown {

		close(dns.stopCh)
		glog.Infof("shutting down controller queues")
		dns.shutdown = true

		return nil
	}

	return fmt.Errorf("shutdown already in progress")
}

// Run starts the controller.
func (dns *dnsController) Run() {
	glog.Infof("starting coredns controller")

	go dns.podController.Run(dns.stopCh)
	go dns.endpController.Run(dns.stopCh)
	go dns.svcController.Run(dns.stopCh)

	go dns.backend.Start()

	<-dns.stopCh
	glog.Infof("shutting down coredns controller")
}
