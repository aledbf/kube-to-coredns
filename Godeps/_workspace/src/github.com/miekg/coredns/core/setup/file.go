package setup

import (
	"fmt"
	"net"
	"os"

	"github.com/miekg/coredns/middleware"
	"github.com/miekg/coredns/middleware/file"
)

// File sets up the file middleware.
func File(c *Controller) (middleware.Middleware, error) {
	zones, err := fileParse(c)
	if err != nil {
		return nil, err
	}

	// Add startup functions to notify the master(s).
	for _, n := range zones.Names {
		c.Startup = append(c.Startup, func() error {
			zones.Z[n].StartupOnce.Do(func() {
				if len(zones.Z[n].TransferTo) > 0 {
					zones.Z[n].Notify()
				}
				zones.Z[n].Reload(nil)
			})
			return nil
		})
	}

	return func(next middleware.Handler) middleware.Handler {
		return file.File{Next: next, Zones: zones}
	}, nil

}

func fileParse(c *Controller) (file.Zones, error) {
	z := make(map[string]*file.Zone)
	names := []string{}
	for c.Next() {
		if c.Val() == "file" {
			// file db.file [zones...]
			if !c.NextArg() {
				return file.Zones{}, c.ArgErr()
			}
			fileName := c.Val()

			origins := c.ServerBlockHosts
			args := c.RemainingArgs()
			if len(args) > 0 {
				origins = args
			}

			reader, err := os.Open(fileName)
			if err != nil {
				// bail out
				return file.Zones{}, err
			}

			for i, _ := range origins {
				origins[i] = middleware.Host(origins[i]).Normalize()
				zone, err := file.Parse(reader, origins[i], fileName)
				if err == nil {
					z[origins[i]] = zone
				} else {
					return file.Zones{}, err
				}
				names = append(names, origins[i])
			}

			noReload := false
			for c.NextBlock() {
				t, _, e := transferParse(c)
				if e != nil {
					return file.Zones{}, e
				}
				switch c.Val() {
				case "no_reload":
					noReload = true
				}
				// discard from, here, maybe check and show log when we do?
				for _, origin := range origins {
					if t != nil {
						z[origin].TransferTo = append(z[origin].TransferTo, t...)
					}
					z[origin].NoReload = noReload
				}
			}
		}
	}
	return file.Zones{Z: z, Names: names}, nil
}

// transferParse parses transfer statements: 'transfer to [address...]'.
func transferParse(c *Controller) (tos, froms []string, err error) {
	what := c.Val()
	if !c.NextArg() {
		return nil, nil, c.ArgErr()
	}
	value := c.Val()
	switch what {
	case "transfer":
		if value == "to" {
			tos = c.RemainingArgs()
			for i, _ := range tos {
				if tos[i] != "*" {
					if x := net.ParseIP(tos[i]); x == nil {
						return nil, nil, fmt.Errorf("must specify an IP addres: `%s'", tos[i])
					}
					tos[i] = middleware.Addr(tos[i]).Normalize()
				}
			}
		}
		if value == "from" {
			froms = c.RemainingArgs()
			for i, _ := range froms {
				if froms[i] != "*" {
					if x := net.ParseIP(froms[i]); x == nil {
						return nil, nil, fmt.Errorf("must specify an IP addres: `%s'", froms[i])
					}
					froms[i] = middleware.Addr(froms[i]).Normalize()
				} else {
					return nil, nil, fmt.Errorf("can't use '*' in transfer from")
				}
			}
		}
	}
	return
}
