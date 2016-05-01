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
	"hash/fnv"
	"reflect"
	"strings"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/cache"
)

const (
	// A subdomain added to the user specified domain for all services.
	serviceSubdomain = "svc"
	// A subdomain added to the user specified dmoain for all pods.
	podSubdomain = "pod"
)

// Generates records for a headless service.
func (dns *dnsController) newHeadlessService(subdomain string, service *api.Service) error {
	// Create an A record for every pod in the service.
	// This record must be periodically updated.
	// Format is as follows:
	// For a service x, with pods a and b create DNS records,
	// a.x.ns.domain. and, b.x.ns.domain.
	key, err := cache.MetaNamespaceKeyFunc(service)
	if err != nil {
		return err
	}
	e, exists, err := dns.endpLister.GetByKey(key)
	if err != nil {
		return fmt.Errorf("failed to get endpoints object from endpoints store - %v", err)
	}
	if !exists {
		glog.V(1).Infof("Could not find endpoints for service %q in namespace %q. DNS records will be created once endpoints show up.", service.Name, service.Namespace)
		return nil
	}
	if e, ok := e.(*api.Endpoints); ok {
		return dns.generateRecordsForHeadlessService(subdomain, e, service)
	}
	return nil
}

func (dns *dnsController) generateRecordsForHeadlessService(fqdn string, e *api.Endpoints, svc *api.Service) error {
	for idx := range e.Subsets {
		for subIdx := range e.Subsets[idx].Addresses {
			dns.backend.AddHost(fqdn, e.Subsets[idx].Addresses[subIdx].IP)
			for portIdx := range e.Subsets[idx].Ports {
				endpointPort := &e.Subsets[idx].Ports[portIdx]
				portSegment := buildPortSegmentString(endpointPort.Name, endpointPort.Protocol)
				if portSegment != "" {
					// auto-generated-name.my-svc.my-namespace.svc.cluster.local
					randomName := buildDNSNameString(fqdn, getHash(fqdn+"-"+e.Subsets[idx].Addresses[subIdx].IP))
					dns.generateSRVRecord(fqdn, portSegment, randomName, endpointPort.Port)
				}
			}
		}
	}

	return nil
}

func (dns *dnsController) getServiceFromEndpoints(e *api.Endpoints) (*api.Service, error) {
	key, err := cache.MetaNamespaceKeyFunc(e)
	if err != nil {
		return nil, err
	}
	obj, exists, err := dns.svcLister.GetByKey(key)
	if err != nil {
		return nil, fmt.Errorf("failed to get service object from services store - %v", err)
	}
	if !exists {
		glog.V(1).Infof("could not find service for endpoint %q in namespace %q", e.Name, e.Namespace)
		return nil, nil
	}
	if svc, ok := obj.(*api.Service); ok {
		return svc, nil
	}
	return nil, fmt.Errorf("got a non service object in services store %v", obj)
}

func (dns *dnsController) addDNSUsingEndpoints(subdomain string, e *api.Endpoints) error {
	svc, err := dns.getServiceFromEndpoints(e)
	if err != nil {
		return err
	}
	if svc == nil || api.IsServiceIPSet(svc) {
		// No headless service found corresponding to endpoints object.
		return nil
	}

	for _, ss := range e.Subsets {
		for _, addr := range ss.Addresses {
			// Remove existing DNS entry.
			dns.backend.RemoveHost(subdomain, addr.IP)
		}
	}

	return dns.generateRecordsForHeadlessService(subdomain, e, svc)
}

func (dns *dnsController) handleEndpointAdd(obj interface{}) {
	if e, ok := obj.(*api.Endpoints); ok {
		name := buildDNSNameString(dns.domain, serviceSubdomain, e.Namespace, e.Name)
		dns.addDNSUsingEndpoints(name, e)
	}
}

func (dns *dnsController) handlePodCreate(obj interface{}) {
	if e, ok := obj.(*api.Pod); ok {
		// If the pod ip is not yet available, do not attempt to create.
		if e.Status.PodIP != "" {
			fqdn := buildDNSNameString(dns.domain, podSubdomain, e.Namespace, santizeIP(e.Status.PodIP))
			dns.backend.AddHost(fqdn, e.Status.PodIP)
		}
	}
}

func (dns *dnsController) handlePodUpdate(old interface{}, new interface{}) {
	oldPod, okOld := old.(*api.Pod)
	newPod, okNew := new.(*api.Pod)

	// Validate that the objects are good
	if okOld && okNew {
		if oldPod.Status.PodIP != newPod.Status.PodIP {
			dns.handlePodDelete(oldPod)
			dns.handlePodCreate(newPod)
		}
	} else if okNew {
		dns.handlePodCreate(newPod)
	} else if okOld {
		dns.handlePodDelete(oldPod)
	}
}

func (dns *dnsController) handlePodDelete(obj interface{}) {
	if e, ok := obj.(*api.Pod); ok {
		if e.Status.PodIP != "" {
			fqdn := buildDNSNameString(dns.domain, podSubdomain, e.Namespace, santizeIP(e.Status.PodIP))
			dns.backend.RemoveHost(fqdn, e.Status.PodIP)
		}
	}
}

func (dns *dnsController) generateRecordsForPortalService(fqdn string, service *api.Service) error {
	dns.backend.AddHost(fqdn, service.Spec.ClusterIP)
	// Generate SRV Records
	for i := range service.Spec.Ports {
		port := &service.Spec.Ports[i]
		portSegment := buildPortSegmentString(port.Name, port.Protocol)
		if portSegment != "" {
			dns.generateSRVRecord(fqdn, portSegment, fqdn, port.Port)
		}
	}
	return nil
}

func santizeIP(ip string) string {
	return strings.Replace(ip, ".", "-", -1)
}

func buildPortSegmentString(portName string, portProtocol api.Protocol) string {
	if portName == "" {
		// we don't create a random name
		return ""
	}

	if portProtocol == "" {
		glog.Errorf("Port Protocol not set. port segment string cannot be created.")
		return ""
	}

	return fmt.Sprintf("_%s._%s", portName, strings.ToLower(string(portProtocol)))
}

func (dns *dnsController) generateSRVRecord(subdomain, portSegment, cName string, portNumber int32) {
	recordKey := buildDNSNameString(subdomain, portSegment)
	dns.backend.AddSrv(recordKey, cName, portNumber)
}

func (dns *dnsController) addDNS(fqdn string, service *api.Service) error {
	if len(service.Spec.Ports) == 0 {
		glog.Fatalf("Unexpected service with no ports: %v", service)
	}
	// if ClusterIP is not set, a DNS entry should not be created
	if !api.IsServiceIPSet(service) {
		return dns.newHeadlessService(fqdn, service)
	}
	return dns.generateRecordsForPortalService(fqdn, service)
}

func buildDNSNameString(labels ...string) string {
	var res string
	for _, label := range labels {
		if res == "" {
			res = label
		} else {
			res = fmt.Sprintf("%s.%s", label, res)
		}
	}
	return res
}

func (dns *dnsController) handleServiceCreate(obj interface{}) {
	if s, ok := obj.(*api.Service); ok {
		fqdn := buildDNSNameString(dns.domain, serviceSubdomain, s.Namespace, s.Name)
		dns.addDNS(fqdn, s)
	}
}

func (dns *dnsController) handleServiceRemove(obj interface{}) {
	if s, ok := obj.(*api.Service); ok {
		fqdn := buildDNSNameString(dns.domain, serviceSubdomain, s.Namespace, s.Name)
		dns.backend.RemoveHost(fqdn, s.Spec.ClusterIP)
	}
}

func (dns *dnsController) handleServiceUpdate(oldObj, newObj interface{}) {
	if !reflect.DeepEqual(oldObj, newObj) {
		dns.handleServiceRemove(oldObj)
		dns.handleServiceCreate(newObj)
	}
}

func getHash(text string) string {
	h := fnv.New32a()
	h.Write([]byte(text))
	return fmt.Sprintf("%x", h.Sum32())
}
