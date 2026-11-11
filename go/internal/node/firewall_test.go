package node

import (
	"net/netip"
	"strings"
	"testing"
)

func TestTheFirewallTouchesOnlyMachineBridges(t *testing.T) {
	rules, err := FirewallRules(FirewallConfig{Pool: netip.MustParsePrefix("10.250.0.0/16"), OnLink: []netip.Prefix{netip.MustParsePrefix("203.0.114.0/26")}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.SplitN(rules, "table inet cloudex\n", 2)[1], "delete table inet cloudex\n") {
		t.Fatal("loading must replace the table, and only it")
	}
	if strings.Contains(rules, "flush ruleset") || strings.Contains(rules, "table ip ") || strings.Contains(rules, "policy drop") {
		t.Fatal("the rules reach outside their own table")
	}
	var chainRules []string
	for _, l := range strings.Split(rules, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "iifname") || strings.HasPrefix(l, "oifname") {
			chainRules = append(chainRules, l)
			if !strings.Contains(l, `"cx*"`) {
				t.Errorf("a rule that isn't about machine bridges: %s", l)
			}
			if strings.Contains(l, " accept") && strings.Contains(l, "drop") {
				t.Errorf("%s", l)
			}
		}
	}
	// In this order: a source from another bridge (review H6), IPv6, the
	// agent on a machine's own bridge, answers, nothing in, machine to
	// machine, Docker's bridges, the blocked ranges; and into the host: a
	// source from another bridge, answers, the agent's canary, nothing else.
	order := []string{"fib saddr . iif oif missing", "meta nfproto ipv6", "meta nfproto ipv6", "fib daddr . iif oif exists", "ct state established,related",
		`oifname "cx*" counter drop`, `iifname "cx*" oifname "cx*" counter drop`, `oifname "docker0"`, `oifname "br-*"`,
		"@blocked4", "@onlink4", "fib saddr . iif oif missing", "ct state established,related", "tcp dport 9 counter reject with icmp type admin-prohibited", `iifname "cx*" counter drop`}
	if len(chainRules) != len(order) {
		t.Fatalf("%d rules:\n%s", len(chainRules), strings.Join(chainRules, "\n"))
	}
	for i, want := range order {
		if !strings.Contains(chainRules[i], want) {
			t.Errorf("rule %d is %q, want %q", i, chainRules[i], want)
		}
	}
	for _, r := range []string{"10.0.0.0/8", "100.64.0.0/10", "169.254.0.0/16", "172.16.0.0/12", "192.168.0.0/16", "168.63.129.16/32", "224.0.0.0/4"} {
		if !strings.Contains(rules, r) {
			t.Errorf("%s isn't blocked", r)
		}
	}
	if !strings.Contains(rules, "203.0.114.0/26") || !strings.Contains(rules, "priority filter - 10") {
		t.Error("no on-link prefix, or the wrong priority")
	}
}

func TestTheFirewallNeedsAPrivatePool(t *testing.T) {
	if _, err := FirewallRules(FirewallConfig{Pool: netip.MustParsePrefix("8.8.0.0/16")}); err == nil {
		t.Fatal("a public pool")
	}
	if _, err := FirewallRules(FirewallConfig{Pool: netip.MustParsePrefix("10.250.0.0/16"), Block: []netip.Prefix{netip.MustParsePrefix("fd00::/8")}}); err == nil {
		t.Fatal("an IPv6 block")
	}
	rules, err := FirewallRules(FirewallConfig{Pool: netip.MustParsePrefix("10.250.0.0/16"), Block: []netip.Prefix{netip.MustParsePrefix("8.8.4.0/24")}})
	if err != nil || !strings.Contains(rules, "8.8.4.0/24") {
		t.Fatal(err)
	}
}

func TestOnlyPublicOnLinkPrefixesNeedNaming(t *testing.T) {
	got := PublicOnLink([]netip.Prefix{netip.MustParsePrefix("172.31.32.0/20"), netip.MustParsePrefix("100.73.0.0/16"),
		netip.MustParsePrefix("203.0.114.0/26"), netip.MustParsePrefix("0.0.0.0/0"), netip.MustParsePrefix("2001:db8::/64")})
	if len(got) != 1 || got[0].String() != "203.0.114.0/26" {
		t.Fatalf("%v", got)
	}
}
