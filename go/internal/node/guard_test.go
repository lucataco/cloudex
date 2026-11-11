package node

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// fakeListener hands out connections from a peer, as a machine network's would.
type fakeListener struct {
	local, peer netip.AddrPort
	closed      chan struct{}
}

type fakeConn struct {
	net.Conn
	l, r   net.Addr
	closed *bool
}

func (c *fakeConn) LocalAddr() net.Addr  { return c.l }
func (c *fakeConn) RemoteAddr() net.Addr { return c.r }
func (c *fakeConn) Close() error         { *c.closed = true; return nil }

func (f *fakeListener) Accept() (net.Conn, error) {
	b := false
	return &fakeConn{l: net.TCPAddrFromAddrPort(f.local), r: net.TCPAddrFromAddrPort(f.peer), closed: &b}, nil
}
func (f *fakeListener) Close() error   { return nil }
func (f *fakeListener) Addr() net.Addr { return net.TCPAddrFromAddrPort(f.local) }

// Review H4: one machine network holds at most EgressConnsMax connections;
// more are closed as they're accepted, and closing one frees its place.
func TestAMachineNetworkHoldsAtMostItsConnections(t *testing.T) {
	old := EgressConnsMax
	defer func() { EgressConnsMax = old }()
	EgressConnsMax = 3
	n, _ := NetFor("cx-abc123", netip.MustParsePrefix("10.250.0.16/28"))
	en := &egressNet{sandbox: "cx-abc123", net: n}
	e := &Egress{}
	l := &machineListener{Listener: &fakeListener{local: netip.AddrPortFrom(n.Agent, 80), peer: netip.AddrPortFrom(n.Machine, 40000)}, n: n, e: e, en: en}
	var held []net.Conn
	for i := 0; i < 3; i++ {
		c, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	if en.conns.Load() != 3 {
		t.Fatalf("%d open", en.conns.Load())
	}
	// The fourth is closed inside Accept, which goes on to the next: it
	// returns once one is free.
	got := make(chan net.Conn)
	go func() { c, _ := l.Accept(); got <- c }()
	time.Sleep(50 * time.Millisecond)
	if en.full.Load() == 0 {
		t.Fatal("the fourth connection wasn't refused")
	}
	held[0].Close()
	held[0].Close() // twice gives back one place
	select {
	case c := <-got:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("a freed place wasn't used")
	}
	if n := en.conns.Load(); n != 2 {
		t.Fatalf("%d open after closing two of four", n)
	}
}

// Review H8: networks are handed out in turn, not the lowest free one.
func TestSubnetsGoRoundThePool(t *testing.T) {
	pool := netip.MustParsePrefix("10.250.0.0/16")
	a, _ := NextSubnetAfter(pool, nil, netip.Prefix{})
	b, _ := NextSubnetAfter(pool, nil, a) // a was deleted meanwhile: still not reused
	if a.String() != "10.250.0.0/28" || b.String() != "10.250.0.16/28" {
		t.Fatalf("%s %s", a, b)
	}
	c, _ := NextSubnetAfter(pool, []netip.Prefix{netip.MustParsePrefix("10.250.0.32/28")}, b)
	if c.String() != "10.250.0.48/28" {
		t.Fatalf("skips one in use: %s", c)
	}
	last := netip.MustParsePrefix("10.250.255.240/28")
	d, _ := NextSubnetAfter(pool, nil, last)
	if d.String() != "10.250.0.0/28" {
		t.Fatalf("wraps: %s", d)
	}
	if _, err := NextSubnetAfter(netip.MustParsePrefix("10.250.0.0/24"), []netip.Prefix{netip.MustParsePrefix("10.250.0.0/24")}, a); err != ErrPoolFull {
		t.Fatalf("full: %v", err)
	}
}

func TestDockerBefore28CantRunMachines(t *testing.T) {
	for v, ok := range map[string]bool{"27.5.1": false, "24.0.7": false, "28.0.0": true, "29.9.3": true, "": true} {
		if err := DockerTooOld(v); (err == nil) != ok {
			t.Errorf("%q: %v", v, err)
		} else if err != nil && !strings.Contains(err.Error(), "Docker 28 or later") {
			t.Errorf("%q: %v", v, err)
		}
	}
}

// Review H2: the cap goes in only where the storage driver has quotas.
func TestTheDiskCapNeedsADriverWithQuotas(t *testing.T) {
	r := &Runtime{DiskGB: 50}
	for _, c := range []struct {
		info DockerInfo
		want int
	}{
		{DockerInfo{Driver: "overlayfs"}, 0},
		{DockerInfo{Driver: "overlay2", DriverStatus: [][2]string{{"Backing Filesystem", "extfs"}}}, 0},
		{DockerInfo{Driver: "overlay2", DriverStatus: [][2]string{{"Backing Filesystem", "xfs"}}}, 50},
		{DockerInfo{Driver: "btrfs"}, 50},
	} {
		if got := r.diskCap(&c.info); got != c.want {
			t.Errorf("%+v: %d", c.info, got)
		}
	}
	n, _ := NetFor("cx-abc123", netip.MustParsePrefix("10.250.0.16/28"))
	b, err := MachineSpec(MachineRequest{Sandbox: "cx-abc123"}, MachineHost{NodeID: "nd-x", Image: "i", Network: n, DiskGB: 50})
	if err != nil || b.HostConfig.StorageOpt["size"] != "50G" {
		t.Fatalf("%v %v", b.HostConfig.StorageOpt, err)
	}
	b, _ = MachineSpec(MachineRequest{Sandbox: "cx-abc123"}, MachineHost{NodeID: "nd-x", Image: "i", Network: n})
	if b.HostConfig.StorageOpt != nil {
		t.Fatal("a cap without one asked for")
	}
	if (&Runtime{}).minFree() != 10 || (&Runtime{KeepFreeGB: 20}).minFree() != 25 {
		t.Fatal("minFree")
	}
}
