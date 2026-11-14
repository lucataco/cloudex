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
// bridges and veths, the machine networks, other container and VM bridges,
// and (review D-L1) VPN and overlay tunnels, whose peers aren't the user's
// LAN: WireGuard, OpenVPN and other tun/tap, ZeroTier, PPP, NetBird, Nebula.
// --direct-addr still names an address on any of them.
var skipIfaces = []string{"docker", "br-", "veth", "cx", "cloudex", "virbr", "lxcbr", "incusbr", "cni", "flannel", "kube", "wg", "tun", "tap", "zt", "ppp", "wt", "nebula"}

// tailnetIfaces are Tailscale's interfaces (tailscale0, or another name it
// was given that starts so). 100.64/10 elsewhere is carrier-grade NAT on a
// WAN, a Docker network, or another overlay: not the tailnet.
var tailnetIfaces = []string{"tailscale"}

func hasPrefix(name string, ps []string) bool {
	for _, p := range ps {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// DirectAddr is an address ChooseDirectAddrs took, and why.
type DirectAddr struct {
	Addr  netip.Addr
	Iface string
	Kind  string // tailnet or lan
}

// ChooseDirectAddrs is the installer's --direct auto (`cloudex-node
// direct-addrs`, run with --network host): the 100.64/10 IPv4 addresses on
// Tailscale's interface, then the RFC 1918 IPv4 addresses of up,
// non-loopback interfaces that aren't a bridge or veth of Docker, the
// machines or another container runtime, nor a VPN tunnel (review D-L1),
// each sorted, at most DirectAddrsMax.
func ChooseDirectAddrs(ifs []Iface) []netip.Addr {
	var out []netip.Addr
	for _, a := range ChooseDirectAddrsWhy(ifs) {
		out = append(out, a.Addr)
	}
	return out
}

// ChooseDirectAddrsWhy is ChooseDirectAddrs with each address's interface.
func ChooseDirectAddrsWhy(ifs []Iface) []DirectAddr {
	var ts, lan []DirectAddr
	seen := map[netip.Addr]bool{}
	for _, i := range ifs {
		if !i.Up || i.Loopback || hasPrefix(i.Name, skipIfaces) {
			continue
		}
		tailnet := hasPrefix(i.Name, tailnetIfaces)
		for _, p := range i.Addrs {
			a := p.Addr().Unmap()
			if !a.Is4() || seen[a] {
				continue
			}
			switch {
			case tailnet && direct.IsTailscale(a) && direct.Usable(a):
				ts = append(ts, DirectAddr{a, i.Name, "tailnet"})
				seen[a] = true
			case !tailnet && direct.Private(a):
				lan = append(lan, DirectAddr{a, i.Name, "lan"})
				seen[a] = true
			}
		}
	}
	less := func(l []DirectAddr) { sort.Slice(l, func(i, j int) bool { return l[i].Addr.Less(l[j].Addr) }) }
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
