package node

import (
	"net/netip"
	"strings"
	"testing"
	"time"
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

func TestASelfTestThatCouldntRunIsNoVerdict(t *testing.T) {
	r := &Runtime{}
	// First, while the image is still being pulled: still unchecked (empty
	// here, which Facts reports as unchecked), with why.
	r.Record(&SelfTestReport{At: 1, Error: "the machine image: pulling x"})
	f := Facts{Isolation: r.iso}
	if f.Isolation.Firewall != "" || len(f.Isolation.Failed) != 1 || !strings.Contains(f.Isolation.Failed[0], "pulling") || f.Isolation.CheckedAt != 0 {
		t.Fatalf("after an error first: %+v", f.Isolation)
	}
	if err := r.isolated(); err == nil || !strings.Contains(err.Error(), "hasn't finished") {
		t.Fatalf("machines while unchecked: %v", err)
	}
	r.Record(&SelfTestReport{At: 2, Pass: true})
	// A later error keeps the pass it had.
	r.Record(&SelfTestReport{At: 3, Error: "Docker didn't answer"})
	f = Facts{Isolation: r.iso}
	if f.Isolation.Firewall != "pass" || f.Isolation.CheckedAt != 2 {
		t.Fatalf("after a pass, then an error: %+v", f.Isolation)
	}
	// Review H3: but not for ever. Errors for longer than VerdictMaxAge
	// after the last verdict leave the node unchecked, refusing machines.
	r.Record(&SelfTestReport{At: 2 + VerdictMaxAge.Milliseconds() + 1, Error: "Docker didn't answer"})
	if r.iso.Firewall != "unchecked" {
		t.Fatalf("an old pass still stands after errors: %+v", r.iso)
	}
	if err := r.isolated(); err == nil {
		t.Fatal("machines without a recent verdict")
	}
	// And a pass that's simply old (self-tests stopped running) refuses too.
	r.Record(&SelfTestReport{At: time.Now().Add(-VerdictMaxAge - time.Minute).UnixMilli(), Pass: true})
	if err := r.isolated(); err == nil || !strings.Contains(err.Error(), "none has run since") {
		t.Fatalf("an old pass: %v", err)
	}
	r.Record(&SelfTestReport{At: time.Now().UnixMilli(), Pass: true})
	if err := r.isolated(); err != nil {
		t.Fatalf("a fresh pass: %v", err)
	}
}

func TestABreachIsOnlyAClosedCheckFoundOpen(t *testing.T) {
	ok := &SelfTestReport{Checks: []Check{{Name: "internet", Want: "open", Got: "timeout"}, {Name: "dns", Want: "resolved", Got: "error"}}}
	if ok.Breach() {
		t.Fatal("an unreachable internet is no breach")
	}
	bad := &SelfTestReport{Checks: []Check{{Name: "internet", Want: "open", Got: "open", OK: true}, {Name: "metadata", Want: "closed", Got: "open"}}}
	if !bad.Breach() {
		t.Fatal("metadata open is a breach")
	}
}
