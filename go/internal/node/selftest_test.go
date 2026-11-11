package node

import (
	"net/netip"
	"strings"
	"testing"
)

func TestAMachineIsKnownByItsNetworkAndItsSource(t *testing.T) {
	a, _ := NetFor("cx-aaaaaa", netip.MustParsePrefix("10.250.0.0/28"))
	b, _ := NetFor("cx-bbbbbb", netip.MustParsePrefix("10.250.0.16/28"))
	nets := []MachineNet{a, b}
	ip := netip.MustParseAddr
	for _, c := range []struct {
		local, peer string
		want        string
	}{
		{"10.250.0.2", "10.250.0.3", "cloudex-cx-aaaaaa"},
		{"10.250.0.18", "10.250.0.19", "cloudex-cx-bbbbbb"},
		{"10.250.0.18", "10.250.0.3", ""}, // A, routed to the agent's address on B (M0 Q4)
		{"10.250.0.2", "10.250.0.19", ""}, // a source from B, arriving at A's address
		{"172.17.0.2", "10.250.0.3", ""},  // the agent's control address
		{"10.250.0.2", "10.250.0.1", ""},  // the host's bridge address
		{"10.250.0.2", "10.250.0.2", ""},  // the agent itself
		{"10.250.0.2", "10.250.0.15", ""}, // the broadcast address
		{"10.250.0.2", "192.168.1.5", ""}, // the LAN
		{"::ffff:10.250.0.2", "::ffff:10.250.0.3", "cloudex-cx-aaaaaa"},
	} {
		n, ok := CallerNetwork(ip(c.local), ip(c.peer), nets)
		if (c.want == "") == ok || (ok && n.Name != c.want) {
			t.Errorf("took %s from %s: %v %s", c.local, c.peer, ok, n.Name)
		}
	}
}

func TestProbeResultsAreReadStrictly(t *testing.T) {
	own, _ := NetFor("cx-t000001", netip.MustParsePrefix("10.250.0.0/28"))
	for _, c := range []struct {
		expect, got string
		ok          bool
	}{
		{"closed", "timeout", true}, {"closed", "unreachable", true},
		{"closed", "refused", false}, // an RST means it got there
		{"closed", "open", false}, {"closed", "err:Permission denied", false},
		{"refused", "open:refused", true}, {"refused", "open:machine cloudex-cx-t000002", false}, {"refused", "refused", false},
		{"own", "open:machine cloudex-cx-t000001", true}, {"own", "open:refused", false},
		{"open", "open", true}, {"open", "timeout", false},
		{"resolved", "resolved:1.2.3.4", true}, {"resolved", "err:x", false},
		{"timeout", "answered", false},
	} {
		if judge(c.expect, c.got, own) != c.ok {
			t.Errorf("%s / %s", c.expect, c.got)
		}
	}
}

func TestAFailedReportNamesItsChecks(t *testing.T) {
	r := &SelfTestReport{Checks: []Check{{Name: "a", OK: true}, {Name: "metadata", Got: "open", OK: false}}}
	if f := r.Failed(); len(f) != 1 || !strings.Contains(f[0], "metadata: open") {
		t.Fatalf("%v", f)
	}
}
