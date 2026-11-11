package node

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"
)

// The host firewall for machine networks (v0.2.2 M4), one nftables table,
// "inet cloudex", that the installer writes to /etc/cloudex-node/firewall.nft
// and a systemd unit (cloudex-node-firewall.service) loads at every boot and
// every Docker restart. It touches only packets that come from or go to a
// bridge whose name starts with "cx", which are the agent's machine networks
// (netalloc.go): everything else on the host, its ssh included, is left alone.
//
// Its chains run at priority filter - 10, before Docker's own. A drop in any
// base chain is final in netfilter, so Docker's rules can't undo one; an
// accept here still lets Docker's chains see the packet.
//
// What it does, for a packet from a machine:
//   - first, in both chains: a source address that isn't on the bridge it
//     came in on (a machine that took another's address) is dropped.
//   - input (to any of the host's own addresses, whichever it names): drop,
//     except the agent's canary port, which is rejected so that the agent
//     can tell the table is there (canary.go).
//   - forward: drop IPv6; accept frames between the machine and the agent on
//     its own bridge, when the bridge is filtered at all (br_netfilter); drop
//     traffic to other machine networks and Docker's other bridges, private,
//     shared, link-local and reserved ranges, cloud metadata, multicast, and
//     the host's on-link prefixes; the internet is what's left.
// And nothing from outside opens a connection to a machine.
//
// The agent can't read these rules (it has no NET_ADMIN on the host), so it
// tests them instead (selftest.go).

// CanaryPort is where the agent's canary knocks on the host from a machine
// network: the firewall answers it with an ICMP admin-prohibited (canary.go).
const CanaryPort = 9

// Blocked4 are the IPv4 ranges a machine may not reach.
var Blocked4 = []string{
	"0.0.0.0/8",        // "this network"
	"10.0.0.0/8",       // RFC 1918
	"100.64.0.0/10",    // shared address space: Tailscale, NetBird, carrier NAT
	"127.0.0.0/8",      // loopback
	"169.254.0.0/16",   // link-local, cloud metadata (169.254.169.254) among it
	"172.16.0.0/12",    // RFC 1918
	"192.0.0.0/24",     // IETF protocol assignments
	"192.0.2.0/24",     // documentation
	"192.88.99.0/24",   // 6to4 relays
	"192.168.0.0/16",   // RFC 1918
	"198.18.0.0/15",    // benchmarking
	"198.51.100.0/24",  // documentation
	"203.0.113.0/24",   // documentation
	"224.0.0.0/4",      // multicast
	"240.0.0.0/4",      // reserved and broadcast
	"168.63.129.16/32", // Azure's WireServer, a metadata service at a public address
}

// FirewallConfig is what the rules depend on.
type FirewallConfig struct {
	Pool   netip.Prefix   // the machine pool, for the comment and a check
	OnLink []netip.Prefix // the host's on-link prefixes that aren't private, as installed
	Block  []netip.Prefix // more prefixes to block (the installer's --block)
}

// FirewallRules is the nftables file. Loading it replaces the table and
// nothing else.
func FirewallRules(c FirewallConfig) (string, error) {
	if err := CheckPool(c.Pool); err != nil {
		return "", err
	}
	blocked := append([]string(nil), Blocked4...)
	for _, p := range c.Block {
		if !p.Addr().Is4() {
			return "", fmt.Errorf("%s: machine networks are IPv4 only, and IPv6 is dropped whole", p)
		}
		blocked = append(blocked, p.Masked().String())
	}
	onlink := []string{}
	for _, p := range c.OnLink {
		if p.Addr().Is4() {
			onlink = append(onlink, p.Masked().String())
		}
	}
	sort.Strings(onlink)
	elems := func(list []string) string {
		if len(list) == 0 {
			return ""
		}
		return "\n\t\telements = { " + strings.Join(list, ",\n\t\t\t     ") + " }"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `# cloudex-node: the firewall for machine networks (bridges named cx*).
# Machine pool %s. Written by the installer (scripts/node-install.sh);
# cloudex-node-firewall.service loads it. Loading it replaces only the table
# inet cloudex; nothing outside the cx* bridges is touched.
table inet cloudex
delete table inet cloudex
table inet cloudex {
	set blocked4 {
		type ipv4_addr; flags interval; auto-merge%s
	}
	# The host's on-link prefixes that aren't private (a VPS's public LAN),
	# as they were when the firewall was loaded.
	set onlink4 {
		type ipv4_addr; flags interval; auto-merge%s
	}

	chain forward {
		type filter hook forward priority filter - 10; policy accept;
		# A machine's packet must come from its own bridge's subnet: under
		# Sysbox root may add another machine's address, and an answer to a
		# forged source would reach that machine as established (review H6).
		iifname "cx*" fib saddr . iif oif missing counter drop comment "a source address that isn't on the bridge it came in on"
		# Machine networks are IPv4 only.
		iifname "cx*" meta nfproto ipv6 counter drop comment "IPv6 from a machine"
		oifname "cx*" meta nfproto ipv6 counter drop comment "IPv6 to a machine"
		# A frame between a machine and the agent on its own bridge, when the
		# host filters bridged traffic (br_netfilter): the route to its
		# destination goes back out the bridge it came in on.
		iifname "cx*" oifname "cx*" fib daddr . iif oif exists counter accept comment "machine and agent on one network"
		oifname "cx*" ct state established,related counter accept comment "answers to a machine"
		oifname "cx*" counter drop comment "nothing else opens a connection to a machine"
		iifname "cx*" oifname "cx*" counter drop comment "machine networks to each other"
		iifname "cx*" oifname "docker0" counter drop comment "Docker's default bridge"
		iifname "cx*" oifname "br-*" counter drop comment "Docker's other networks"
		iifname "cx*" ip daddr @blocked4 counter drop comment "private, shared, link-local, metadata and reserved"
		iifname "cx*" ip daddr @onlink4 counter drop comment "the host's on-link prefixes"
	}

	# Packets to any of the host's own addresses arrive here, whichever
	# address they name, so this covers the host's services on every one.
	chain input {
		type filter hook input priority filter - 10; policy accept;
		iifname "cx*" fib saddr . iif oif missing counter drop comment "a source address that isn't on the bridge it came in on"
		iifname "cx*" ct state established,related counter accept comment "answers to the host"
		# The agent's canary (review H3): it connects here from its address on
		# a machine network every few seconds. This answer says the table is
		# loaded; a refusal or an answer from the host says it's gone.
		iifname "cx*" tcp dport %d counter reject with icmp type admin-prohibited comment "the agent's canary"
		iifname "cx*" counter drop comment "the host itself"
	}
}
`, c.Pool, elems(blocked), elems(onlink), CanaryPort)
	return b.String(), nil
}

// PublicOnLink keeps the prefixes that no entry of Blocked4 covers: the
// on-link prefixes the firewall has to name itself.
func PublicOnLink(routes []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, r := range routes {
		if !r.Addr().Is4() || r.Bits() == 0 {
			continue
		}
		covered := false
		for _, s := range Blocked4 {
			b := netip.MustParsePrefix(s)
			if b.Bits() <= r.Bits() && b.Contains(r.Addr()) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, r.Masked())
		}
	}
	return out
}
