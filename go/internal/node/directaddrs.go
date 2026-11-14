package node

import (
	"net"
	"net/netip"
	"sort"
	"strings"

	"github.com/lucataco/cloudex/go/internal/direct"
)

// Iface is a network interface and its addresses, for ChooseDirectAddrs.
type Iface struct {
	Name     string
	Up       bool
	Loopback bool
	Addrs    []netip.Prefix
}

// skipIfaces are interfaces that aren't the host's own LAN: Docker's
// bridges and veths, the machine networks, other container and VM bridges.
var skipIfaces = []string{"docker", "br-", "veth", "cx", "cloudex", "virbr", "lxcbr", "incusbr", "cni", "flannel", "kube"}

// ChooseDirectAddrs is the installer's --direct auto (`cloudex-node
// direct-addrs`, run with --network host): the RFC 1918 IPv4 addresses of
// up, non-loopback interfaces that aren't a bridge or veth of Docker, the
// machines or another container runtime, and every 100.64/10 address (a
// tailnet's), whatever its interface. Tailnet addresses come first, then the
// LAN's, each sorted, at most DirectAddrsMax.
func ChooseDirectAddrs(ifs []Iface) []netip.Addr {
	var ts, lan []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, i := range ifs {
		if !i.Up || i.Loopback {
			continue
		}
		skip := false
		for _, p := range skipIfaces {
			if strings.HasPrefix(i.Name, p) {
				skip = true
			}
		}
		for _, p := range i.Addrs {
			a := p.Addr().Unmap()
			if !a.Is4() || seen[a] {
				continue
			}
			switch {
			case direct.IsTailscale(a) && direct.Usable(a):
				ts = append(ts, a)
				seen[a] = true
			case skip:
			case direct.Private(a):
				lan = append(lan, a)
				seen[a] = true
			}
		}
	}
	less := func(l []netip.Addr) { sort.Slice(l, func(i, j int) bool { return l[i].Less(l[j]) }) }
	less(ts)
	less(lan)
	out := append(ts, lan...)
	if len(out) > DirectAddrsMax {
		out = out[:DirectAddrsMax]
	}
	return out
}

// HostIfaces reads this network namespace's interfaces.
func HostIfaces() ([]Iface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []Iface
	for _, i := range ifs {
		f := Iface{Name: i.Name, Up: i.Flags&net.FlagUp != 0, Loopback: i.Flags&net.FlagLoopback != 0}
		as, _ := i.Addrs()
		for _, a := range as {
			if p, err := netip.ParsePrefix(a.String()); err == nil {
				f.Addrs = append(f.Addrs, p)
			}
		}
		out = append(out, f)
	}
	return out, nil
}
