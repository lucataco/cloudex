package node

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// fakeEngine plays Docker's container list (with sizes) and stop, for the
// guards: running machines, and what was stopped.
type fakeEngine struct {
	mu      sync.Mutex
	list    []ContainerSummary
	stopped []string
}

func startFakeEngine(t *testing.T, list []ContainerSummary) (*fakeEngine, *Docker) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cxe")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEngine{list: list}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/containers/json":
			json.NewEncoder(w).Encode(f.list)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/stop"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/stop")
			f.stopped = append(f.stopped, id)
			for i := range f.list {
				if f.list[i].ID == id {
					f.list[i].State = "exited"
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return f, &Docker{Socket: sock}
}

func machine(id, sandbox string, sizeRw int64) ContainerSummary {
	return ContainerSummary{ID: id, State: "running", SizeRw: sizeRw, Labels: map[string]string{LabelNode: "nd-test0001", LabelRole: "machine", LabelSandbox: sandbox}}
}

// Review H2: under the floor, the watchdog stops the running machine that
// wrote the most, saying why, and another only while the space keeps falling.
func TestTheDiskWatchdogStopsTheBiggestWriterUnderTheFloor(t *testing.T) {
	f, d := startFakeEngine(t, []ContainerSummary{machine("c1", "cx-aaaaaa", 1e9), machine("c2", "cx-bbbbbb", 7e9)})
	free := 6.0
	r := &Runtime{Docker: d, NodeID: "nd-test0001", FreeSpace: func() float64 { return free }}
	var w diskWatch
	ctx := context.Background()
	r.checkDisk(ctx, &w)
	free = 4.2
	r.checkDisk(ctx, &w)
	free = 4.1 // still under, but not falling: nothing more
	r.checkDisk(ctx, &w)
	f.mu.Lock()
	got := append([]string(nil), f.stopped...)
	f.mu.Unlock()
	if len(got) != 1 || got[0] != "c2" {
		t.Fatalf("stopped %v; want only c2, the biggest writer, once under the floor", got)
	}
	if n := r.note("cx-bbbbbb"); !strings.Contains(n, "under its floor of 5 GB") || !strings.Contains(n, "7.0 GB") {
		t.Fatalf("note %q", n)
	}
	free = 3.5 // falling again: the next one
	r.checkDisk(ctx, &w)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stopped) != 2 || f.stopped[1] != "c1" {
		t.Fatalf("stopped %v", f.stopped)
	}
	// The note stays for the hello until the machine runs again.
	if ms, _ := r.machinesLocked(f); len(ms) != 2 || ms[1].Note == "" {
		t.Fatalf("machines %+v", ms)
	}
}

// machinesLocked is Runtime.Machines with the fake engine's lock released
// for the call (its handler takes it).
func (r *Runtime) machinesLocked(f *fakeEngine) ([]Machine, error) {
	f.mu.Unlock()
	defer f.mu.Lock()
	return r.Machines(context.Background())
}

// Review R3: a removed node's agent stops every running machine, saying why.
func TestARevokedAgentStopsItsMachines(t *testing.T) {
	f, d := startFakeEngine(t, []ContainerSummary{machine("c1", "cx-aaaaaa", 0), machine("c2", "cx-bbbbbb", 0)})
	f.list[1].State = "exited"
	a := &Agent{Runtime: &Runtime{Docker: d, NodeID: "nd-test0001"}, State: State{Dir: t.TempDir()}}
	a.revoked(context.Background(), "this node was removed from the deployment")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stopped) != 1 || f.stopped[0] != "c1" {
		t.Fatalf("stopped %v; want the running one", f.stopped)
	}
	if n := a.Runtime.note("cx-aaaaaa"); !strings.Contains(n, "removed from the deployment") {
		t.Fatalf("note %q", n)
	}
}

// Review H3: a knock the host answers or refuses means the table is gone; one
// from an address the agent doesn't have says nothing.
func TestTheCanaryTellsATableThatsGone(t *testing.T) {
	r := &Runtime{}
	ctx := context.Background()
	lo := netip.MustParseAddr("127.0.0.1")
	if got := r.canary(ctx, map[string]MachineNet{"cx-aaaaaa": {Agent: lo, Gateway: lo}}); got != canaryGone {
		t.Fatalf("refused: %q", got)
	}
	away := netip.MustParseAddr("192.0.2.77")
	if got := r.canary(ctx, map[string]MachineNet{"cx-aaaaaa": {Agent: away, Gateway: lo}}); got != canaryNone {
		t.Fatalf("not on that network: %q", got)
	}
}

// Review H3: only a check that must find something closed, failing, is a
// breach that stops running machines; the internet not answering isn't.
func TestOnlyAnOpenHoleIsABreach(t *testing.T) {
	r := &SelfTestReport{Checks: []Check{{Want: "open", OK: false}, {Want: "resolved", OK: false}}}
	if r.Breach() {
		t.Fatal("the internet down is no breach")
	}
	r.Checks = append(r.Checks, Check{Want: "closed", OK: false})
	if !r.Breach() {
		t.Fatal("a closed check that failed is a breach")
	}
}
