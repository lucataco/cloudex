package node

import (
	"net/netip"
	"os"
	"path/filepath"
	"testing"
)

func TestAMachineNetworkIsLaidOutInItsSlash28(t *testing.T) {
	n, err := NetFor("cx-abc123", netip.MustParsePrefix("10.250.0.32/28"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Name != "cloudex-cx-abc123" || n.Bridge != "cxabc123" || n.Gateway.String() != "10.250.0.33" || n.Agent.String() != "10.250.0.34" ||
		n.Machine.String() != "10.250.0.35" || n.Probe.String() != "10.250.0.36" {
		t.Fatalf("%+v", n)
	}
	for _, bad := range []string{"10.250.0.32/27", "10.250.0.33/28", "fd00::/124"} {
		if _, err := NetFor("cx-abc123", netip.MustParsePrefix(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestBridgeNamesStartWithCxAndFitLinux(t *testing.T) {
	for id, want := range map[string]string{"cx-abc123": "cxabc123", "cx-t0a1b2c": "cx-t0a1b2c"} {
		got, err := BridgeName(id)
		if err != nil || got != want || len(got) > 15 {
			t.Errorf("%s: %q %v", id, got, err)
		}
	}
	for _, bad := range []string{"cx-ABC123", "cx-abc12", "eth0", "cx-tzzzzzz", "cx-abc123x"} {
		if _, err := BridgeName(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestTheNextSubnetSkipsWhatIsUsed(t *testing.T) {
	pool := netip.MustParsePrefix("10.250.0.0/16")
	got, err := NextSubnet(pool, nil)
	if err != nil || got.String() != "10.250.0.0/28" {
		t.Fatalf("%v %v", got, err)
	}
	used := []netip.Prefix{netip.MustParsePrefix("10.250.0.0/28"), netip.MustParsePrefix("10.250.0.16/28"), netip.MustParsePrefix("172.17.0.0/16")}
	if got, _ = NextSubnet(pool, used); got.String() != "10.250.0.32/28" {
		t.Fatalf("got %v", got)
	}
	// A bigger network that overlaps takes all its /28s.
	used = append(used, netip.MustParsePrefix("10.250.0.0/24"))
	if got, _ = NextSubnet(pool, used); got.String() != "10.250.1.0/28" {
		t.Fatalf("got %v", got)
	}
	small := netip.MustParsePrefix("10.250.0.0/24")
	var all []netip.Prefix
	for i := 0; i < 16; i++ {
		s, err := NextSubnet(small, all)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, s)
	}
	if _, err := NextSubnet(small, all); err != ErrPoolFull {
		t.Fatalf("a full pool: %v", err)
	}
}

func TestAPoolMustBePrivateIPv4(t *testing.T) {
	for p, ok := range map[string]bool{"10.250.0.0/16": true, "172.30.0.0/16": true, "10.0.0.0/12": true, "192.168.0.0/16": false,
		"8.8.0.0/16": false, "10.0.0.0/8": false, "10.250.0.0/25": false, "172.32.0.0/16": false, "10.250.0.1/16": false} {
		if err := CheckPool(netip.MustParsePrefix(p)); (err == nil) != ok {
			t.Errorf("%s: %v", p, err)
		}
	}
}

func TestThePoolCollidesWithNoRouteOrNetwork(t *testing.T) {
	p, err := PickPool([]netip.Prefix{netip.MustParsePrefix("172.31.32.0/20"), netip.MustParsePrefix("10.250.3.0/24"), netip.MustParsePrefix("10.251.0.0/16")})
	if err != nil || p.String() != "10.252.0.0/16" {
		t.Fatalf("%v %v", p, err)
	}
	if _, err := PickPool([]netip.Prefix{netip.MustParsePrefix("0.0.0.0/1"), netip.MustParsePrefix("128.0.0.0/1")}); err == nil {
		t.Fatal("picked a pool when everything is taken")
	}
}

func TestHostRoutesReadsProcNetRoute(t *testing.T) {
	route := `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
enp39s0	00000000	01201FAC	0003	0	0	100	00000000	0	0	0
wt0	0000496401	00000000	0001	0	0	0	0000FFFF	0	0	0
wt0	00004964	00000000	0001	0	0	0	0000FFFF	0	0	0
docker0	000011AC	00000000	0001	0	0	0	0000FFFF	0	0	0
enp39s0	00201FAC	00000000	0001	0	0	100	00F0FFFF	0	0	0
`
	p := filepath.Join(t.TempDir(), "route")
	os.WriteFile(p, []byte(route), 0o644)
	got, err := HostRoutes(p)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"100.73.0.0/16", "172.17.0.0/16", "172.31.32.0/20"}
	if len(got) != len(want) {
		t.Fatalf("%v", got)
	}
	for i := range want {
		if got[i].String() != want[i] {
			t.Fatalf("%v", got)
		}
	}
}
