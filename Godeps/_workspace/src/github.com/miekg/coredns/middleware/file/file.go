package file

import (
	"errors"
	"io"
	"log"

	"github.com/miekg/coredns/middleware"

	"github.com/miekg/dns"
	"golang.org/x/net/context"
)

type (
	File struct {
		Next  middleware.Handler
		Zones Zones
	}

	Zones struct {
		Z     map[string]*Zone
		Names []string
	}
)

func (f File) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := middleware.State{W: w, Req: r}

	if state.QClass() != dns.ClassINET {
		return dns.RcodeServerFailure, errors.New("can only deal with ClassINET")
	}
	qname := state.Name()
	zone := middleware.Zones(f.Zones.Names).Matches(qname)
	if zone == "" {
		if f.Next != nil {
			return f.Next.ServeDNS(ctx, w, r)
		}
		return dns.RcodeServerFailure, errors.New("no next middleware found")
	}
	z, ok := f.Zones.Z[zone]
	if !ok {
		return f.Next.ServeDNS(ctx, w, r)
	}
	if z == nil {
		return dns.RcodeServerFailure, nil
	}
	if r.Opcode == dns.OpcodeNotify {
		if z.isNotify(state) {
			m := new(dns.Msg)
			m.SetReply(r)
			m.Authoritative, m.RecursionAvailable, m.Compress = true, true, true
			state.SizeAndDo(m)
			w.WriteMsg(m)

			log.Printf("[INFO] Notify from %s for %s: checking transfer", state.IP(), zone)
			ok, err := z.shouldTransfer()
			if ok {
				z.TransferIn()
			} else {
				log.Printf("[INFO] Notify from %s for %s: no serial increase seen", state.IP(), zone)
			}
			if err != nil {
				log.Printf("[WARNING] Notify from %s for %s: failed primary check: %s", state.IP(), zone, err)
			}
			return dns.RcodeSuccess, nil
		}
		log.Printf("[INFO] Dropping notify from %s for %s", state.IP(), zone)
		return dns.RcodeSuccess, nil
	}

	if z.Expired != nil && *z.Expired {
		log.Printf("[ERROR] Zone %s is expired", zone)
		return dns.RcodeServerFailure, nil
	}

	if state.QType() == dns.TypeAXFR || state.QType() == dns.TypeIXFR {
		xfr := Xfr{z}
		return xfr.ServeDNS(ctx, w, r)
	}

	answer, ns, extra, result := z.Lookup(qname, state.QType(), state.Do())

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative, m.RecursionAvailable, m.Compress = true, true, true
	m.Answer, m.Ns, m.Extra = answer, ns, extra

	switch result {
	case Success:
	case NoData:
	case NameError:
		m.Rcode = dns.RcodeNameError
	case Delegation:
		m.Authoritative = false
	case ServerFailure:
		return dns.RcodeServerFailure, nil
	}

	state.SizeAndDo(m)
	m, _ = state.Scrub(m)
	w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}

// Parse parses the zone in filename and returns a new Zone or an error.
func Parse(f io.Reader, origin, fileName string) (*Zone, error) {
	tokens := dns.ParseZone(f, dns.Fqdn(origin), fileName)
	z := NewZone(origin, fileName)
	for x := range tokens {
		if x.Error != nil {
			log.Printf("[ERROR] Failed to parse `%s': %v", origin, x.Error)
			return nil, x.Error
		}
		if err := z.Insert(x.RR); err != nil {
			return nil, err
		}
	}
	return z, nil
}
