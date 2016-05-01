package file

import (
	"fmt"
	"log"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/miekg/coredns/middleware"
	"github.com/miekg/coredns/middleware/file/tree"

	"github.com/fsnotify/fsnotify"
	"github.com/miekg/dns"
)

type Zone struct {
	origin string
	file   string
	*tree.Tree
	Apex Apex

	TransferTo   []string
	StartupOnce  sync.Once
	TransferFrom []string
	Expired      *bool

	NoReload bool
	reloadMu sync.RWMutex
	// TODO: shutdown watcher channel
}

type Apex struct {
	SOA    *dns.SOA
	NS     []dns.RR
	SIGSOA []dns.RR
	SIGNS  []dns.RR
}

// NewZone returns a new zone.
func NewZone(name, file string) *Zone {
	z := &Zone{origin: dns.Fqdn(name), file: path.Clean(file), Tree: &tree.Tree{}, Expired: new(bool)}
	*z.Expired = false
	return z
}

// Copy copies a zone *without* copying the zone's content. It is not a deep copy.
func (z *Zone) Copy() *Zone {
	z1 := NewZone(z.origin, z.file)
	z1.TransferTo = z.TransferTo
	z1.TransferFrom = z.TransferFrom
	z1.Expired = z.Expired
	z1.Apex = z.Apex
	return z1
}

// Insert inserts r into z.
func (z *Zone) Insert(r dns.RR) error {
	r.Header().Name = strings.ToLower(r.Header().Name)

	switch h := r.Header().Rrtype; h {
	case dns.TypeNS:
		r.(*dns.NS).Ns = strings.ToLower(r.(*dns.NS).Ns)

		if r.Header().Name == z.origin {
			z.Apex.NS = append(z.Apex.NS, r)
			return nil
		}
	case dns.TypeSOA:
		r.(*dns.SOA).Ns = strings.ToLower(r.(*dns.SOA).Ns)
		r.(*dns.SOA).Mbox = strings.ToLower(r.(*dns.SOA).Mbox)

		z.Apex.SOA = r.(*dns.SOA)
		return nil
	case dns.TypeNSEC3, dns.TypeNSEC3PARAM:
		return fmt.Errorf("NSEC3 zone is not supported, dropping")
	case dns.TypeRRSIG:
		x := r.(*dns.RRSIG)
		switch x.TypeCovered {
		case dns.TypeSOA:
			z.Apex.SIGSOA = append(z.Apex.SIGSOA, x)
			return nil
		case dns.TypeNS:
			if r.Header().Name == z.origin {
				z.Apex.SIGNS = append(z.Apex.SIGNS, x)
				return nil
			}
		}
	case dns.TypeCNAME:
		r.(*dns.CNAME).Target = strings.ToLower(r.(*dns.CNAME).Target)
	case dns.TypeMX:
		r.(*dns.MX).Mx = strings.ToLower(r.(*dns.MX).Mx)
	case dns.TypeSRV:
		r.(*dns.SRV).Target = strings.ToLower(r.(*dns.SRV).Target)
	}
	z.Tree.Insert(r)
	return nil
}

// Delete deletes r from z.
func (z *Zone) Delete(r dns.RR) { z.Tree.Delete(r) }

// TransferAllowed checks if incoming request for transferring the zone is allowed according to the ACLs.
func (z *Zone) TransferAllowed(state middleware.State) bool {
	for _, t := range z.TransferTo {
		if t == "*" {
			return true
		}
	}
	// TODO(miek): future matching against IP/CIDR notations
	return false
}

// All returns all records from the zone, the first record will be the SOA record,
// otionally followed by all RRSIG(SOA)s.
func (z *Zone) All() []dns.RR {
	z.reloadMu.RLock()
	defer z.reloadMu.RUnlock()

	records := []dns.RR{}
	allNodes := z.Tree.All()
	for _, a := range allNodes {
		records = append(records, a.All()...)
	}

	if len(z.Apex.SIGNS) > 0 {
		records = append(z.Apex.SIGNS, records...)
	}
	records = append(z.Apex.NS, records...)

	if len(z.Apex.SIGSOA) > 0 {
		records = append(z.Apex.SIGSOA, records...)
	}
	return append([]dns.RR{z.Apex.SOA}, records...)
}

func (z *Zone) Reload(shutdown chan bool) error {
	if z.NoReload {
		return nil
	}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	err = watcher.Add(path.Dir(z.file))
	if err != nil {
		return err
	}

	go func() {
		// TODO(miek): needs to be killed on reload.
		for {
			select {
			case event := <-watcher.Events:
				if path.Clean(event.Name) == z.file {
					reader, err := os.Open(z.file)
					if err != nil {
						log.Printf("[ERROR] Failed to open `%s' for `%s': %v", z.file, z.origin, err)
						continue
					}
					z.reloadMu.Lock()
					zone, err := Parse(reader, z.origin, z.file)
					if err != nil {
						log.Printf("[ERROR] Failed to parse `%s': %v", z.origin, err)
						z.reloadMu.Unlock()
						continue
					}
					// copy elements we need
					z.Apex = zone.Apex
					z.Tree = zone.Tree
					z.reloadMu.Unlock()
					log.Printf("[INFO] Successfully reloaded zone `%s'", z.origin)
					z.Notify()
				}
			case <-shutdown:
				watcher.Close()
				return
			}
		}
	}()
	return nil
}
