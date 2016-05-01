package tree

import (
	"github.com/miekg/coredns/middleware"
	"github.com/miekg/dns"
)

type Elem struct {
	m map[uint16][]dns.RR
}

// newElem returns a new elem.
func newElem(rr dns.RR) *Elem {
	e := Elem{m: make(map[uint16][]dns.RR)}
	e.m[rr.Header().Rrtype] = []dns.RR{rr}
	return &e
}

// Types returns the RRs with type qtype from e.
func (e *Elem) Types(qtype uint16) []dns.RR {
	if rrs, ok := e.m[qtype]; ok {
		return rrs
	}
	// nodata
	return nil
}

// All returns all RRs from e, regardless of type.
func (e *Elem) All() []dns.RR {
	list := []dns.RR{}
	for _, rrs := range e.m {
		list = append(list, rrs...)
	}
	return list
}

// Name returns the name for this node.
func (e *Elem) Name() string {
	for _, rrs := range e.m {
		return rrs[0].Header().Name
	}
	return ""
}

// Insert inserts rr into e. If rr is equal to existing rrs this is a noop.
func (e *Elem) Insert(rr dns.RR) {
	t := rr.Header().Rrtype
	if e.m == nil {
		e.m = make(map[uint16][]dns.RR)
		e.m[t] = []dns.RR{rr}
		return
	}
	rrs, ok := e.m[t]
	if !ok {
		e.m[t] = []dns.RR{rr}
		return
	}
	for _, er := range rrs {
		if equalRdata(er, rr) {
			return
		}
	}

	rrs = append(rrs, rr)
	e.m[t] = rrs
}

// Delete removes rr from e. When e is empty after the removal the returned bool is true.
func (e *Elem) Delete(rr dns.RR) (empty bool) {
	if e.m == nil {
		return true
	}

	t := rr.Header().Rrtype
	rrs, ok := e.m[t]
	if !ok {
		return
	}

	for i, er := range rrs {
		if equalRdata(er, rr) {
			rrs = removeFromSlice(rrs, i)
			e.m[t] = rrs
			empty = len(rrs) == 0
			if empty {
				delete(e.m, t)
			}
			return
		}
	}
	return
}

// Less is a tree helper function that calls middleware.Less.
func Less(a *Elem, name string) int { return middleware.Less(name, a.Name()) }

// Assuming the same type and name this will check if the rdata is equal as well.
func equalRdata(a, b dns.RR) bool {
	switch x := a.(type) {
	// TODO(miek): more types, i.e. all types. + tests for this.
	case *dns.A:
		return x.A.Equal(b.(*dns.A).A)
	case *dns.AAAA:
		return x.AAAA.Equal(b.(*dns.AAAA).AAAA)
	case *dns.MX:
		if x.Mx == b.(*dns.MX).Mx && x.Preference == b.(*dns.MX).Preference {
			return true
		}
	}
	return false
}

// removeFromSlice removes index i from the slice.
func removeFromSlice(rrs []dns.RR, i int) []dns.RR {
	if i >= len(rrs) {
		return rrs
	}
	rrs = append(rrs[:i], rrs[i+1:]...)
	return rrs
}
