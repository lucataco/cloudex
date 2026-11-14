package node

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lucataco/cloudex/go/internal/direct"
)

const testNode = "nd-abcdefgh"

var testSecret = []byte("0123456789abcdef0123456789abcdef")

// testDirect is a Direct with its secret, as after a welcome, whose clock is
// now and whose Worker is skew ahead.
func testDirect(now time.Time, skew time.Duration) *Direct {
	d := &Direct{Config: DirectConfig{Port: 7022}, NodeID: testNode, Now: func() time.Time { return now }}
	d.init()
	d.Welcome(&WelcomeDirect{Secret: base64.StdEncoding.EncodeToString(testSecret), Now: now.Add(skew).UnixMilli()})
	return d
}

func claims(exp time.Time) direct.Claims {
	return direct.Claims{Node: testNode, Sandbox: "cx-abc123", Port: 22, Session: direct.NewSession(), Exp: exp.Unix()}
}

func TestAdmit(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	d := testDirect(now, 0)
	c := claims(now.Add(60 * time.Second))
	tk := direct.Mint(testSecret, c)
	if _, why := d.admit(tk); why != "" {
		t.Fatalf("a good ticket: %s", why)
	}
	if _, why := d.admit(tk); why != "used already" {
		t.Fatalf("a replay: %q", why)
	}
	other := c
	other.Node, other.Session = "nd-zzzzzzzz", direct.NewSession()
	if _, why := d.admit(direct.Mint(testSecret, other)); why != "a ticket for another node" {
		t.Fatalf("another node: %q", why)
	}
	if _, why := d.admit(direct.Mint([]byte("another secret, 32 bytes long...."), claims(now.Add(time.Minute)))); why != direct.ErrBadMAC.Error() {
		t.Fatalf("another secret: %q", why)
	}
	b := []byte(direct.Mint(testSecret, claims(now.Add(time.Minute))))
	b[3] ^= 1
	if _, why := d.admit(string(b)); why == "" {
		t.Fatal("an altered ticket passed")
	}
	// Expiry by the Worker's clock, with the grace.
	if _, why := d.admit(direct.Mint(testSecret, claims(now.Add(-DirectGrace+time.Second)))); why != "" {
		t.Fatalf("within the grace: %q", why)
	}
	if _, why := d.admit(direct.Mint(testSecret, claims(now.Add(-DirectGrace)))); why != "expired" {
		t.Fatalf("past the grace: %q", why)
	}
	// A node whose clock is 5 minutes slow judges by the Worker's.
	slow := testDirect(now, 5*time.Minute)
	if _, why := slow.admit(direct.Mint(testSecret, claims(now.Add(time.Minute)))); why != "expired" {
		t.Fatalf("a slow node clock: %q", why)
	}
	if _, why := slow.admit(direct.Mint(testSecret, claims(now.Add(5*time.Minute+30*time.Second)))); why != "" {
		t.Fatalf("a slow node clock, a fresh ticket: %q", why)
	}
	// Without a secret (no welcome with direct), nothing passes.
	none := &Direct{Config: DirectConfig{Port: 7022}, NodeID: testNode}
	none.init()
	if _, why := none.admit(tk); why == "" {
		t.Fatal("a ticket passed with no secret")
	}
}

func TestSeenBound(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := now
	d := testDirect(now, 0)
	d.Now = func() time.Time { return clock }
	for i := 0; i < DirectSeenMax; i++ {
		if _, why := d.admit(direct.Mint(testSecret, claims(now.Add(time.Minute)))); why != "" {
			t.Fatalf("ticket %d: %s", i, why)
		}
	}
	if _, why := d.admit(direct.Mint(testSecret, claims(now.Add(time.Minute)))); !strings.HasPrefix(why, "busy") {
		t.Fatalf("over the bound: %q", why)
	}
	// Each id is kept until its ticket expires (with the grace), then forgotten.
	clock = now.Add(time.Minute + DirectGrace + time.Second)
	if _, why := d.admit(direct.Mint(testSecret, claims(clock.Add(time.Minute)))); why != "" {
		t.Fatalf("after expiry: %q", why)
	}
	if len(d.seen) != 1 {
		t.Fatalf("%d ids kept", len(d.seen))
	}
}

func TestEndRefusesUnusedTickets(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	d := testDirect(now, 0)
	c := claims(now.Add(time.Minute))
	a, b := net.Pipe()
	defer b.Close()
	d.mu.Lock()
	d.sessions["1111111111111111"] = &directSession{id: "1111111111111111", sandbox: c.Sandbox, conn: a}
	d.sessions["2222222222222222"] = &directSession{id: "2222222222222222", sandbox: "cx-other1", conn: a}
	d.mu.Unlock()
	n := d.End(DirectEnd{Sandbox: c.Sandbox, Sessions: []string{"1111111111111111", "2222222222222222", c.Session}})
	if n != 1 {
		t.Fatalf("closed %d sessions; want only the one of that sandbox", n)
	}
	if _, why := d.admit(direct.Mint(testSecret, c)); why != "used already" {
		t.Fatalf("an unused ticket of an ended token: %q", why)
	}
}

func TestClientAllowed(t *testing.T) {
	pool := netip.MustParsePrefix("10.250.0.0/16")
	for a, ok := range map[string]bool{
		"192.168.65.1": true, "100.80.14.76": true, "10.1.2.3": true,
		// The agent network's gateway: a tailnet client behind Tailscale's SNAT.
		"172.18.0.1": true, "10.250.3.3": false, "8.8.8.8": false, "127.0.0.1": false, "0.0.0.0": false, "169.254.1.1": false,
	} {
		if err := ClientAllowed(netip.MustParseAddr(a), pool); (err == nil) != ok {
			t.Errorf("%s: %v", a, err)
		}
	}
}

func TestBindings(t *testing.T) {
	u, p := Bindings([]PortBinding{{"192.168.65.2", "7022"}, {"100.124.24.58", "7022"}, {"192.168.65.2", "7022"}}, 7022)
	if strings.Join(u, ",") != "192.168.65.2,100.124.24.58" || len(p) != 0 {
		t.Fatalf("usable %v public %v", u, p)
	}
	u, p = Bindings([]PortBinding{{"0.0.0.0", "7022"}, {"::", "7022"}, {"", "7022"}, {"8.8.8.8", "7022"}, {"10.0.0.1", "7023"}}, 7022)
	if len(u) != 0 || len(p) != 5 {
		t.Fatalf("usable %v public %v", u, p)
	}
}

// The sidecar is the only container shape besides MachineSpec: no Docker
// socket, no host paths, no capabilities, ports only on the given addresses.
func TestRelaySpec(t *testing.T) {
	ctl := netip.MustParseAddr("172.18.0.2")
	addrs := []netip.Addr{netip.MustParseAddr("192.168.65.2"), netip.MustParseAddr("100.124.24.58")}
	b, err := RelaySpec("sha256:abc", ctl, addrs, 2222)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(b)
	var m map[string]any
	json.Unmarshal(raw, &m)
	hc := m["HostConfig"].(map[string]any)
	for _, k := range []string{"Binds", "Mounts", "Devices", "CapAdd", "Privileged", "PidMode", "IpcMode", "UsernsMode", "VolumesFrom", "Volumes", "NetworkMode:host"} {
		if _, ok := hc[k]; ok {
			t.Errorf("HostConfig has %s", k)
		}
		if _, ok := m[k]; ok {
			t.Errorf("the body has %s", k)
		}
	}
	if strings.Contains(string(raw), "docker.sock") {
		t.Error("the Docker socket is in the spec")
	}
	if hc["NetworkMode"] != AgentNetwork || hc["ReadonlyRootfs"] != true || fmt.Sprint(hc["CapDrop"]) != "[ALL]" || fmt.Sprint(hc["SecurityOpt"]) != "[no-new-privileges]" {
		t.Errorf("HostConfig %v", hc)
	}
	if b.HostConfig.Memory != 64<<20 || b.HostConfig.PidsLimit != 64 || b.HostConfig.RestartPolicy.Name != "unless-stopped" {
		t.Errorf("limits %+v", b.HostConfig)
	}
	pb := b.HostConfig.PortBindings
	if len(pb) != 1 || len(pb["7022/tcp"]) != 2 || pb["7022/tcp"][0] != (PortBinding{"192.168.65.2", "2222"}) || pb["7022/tcp"][1] != (PortBinding{"100.124.24.58", "2222"}) {
		t.Errorf("bindings %v", pb)
	}
	if strings.Join(b.Cmd, " ") != "relay --to 172.18.0.2:7022" || b.Labels[LabelAgent] != "1" || b.Labels[LabelRole] != "direct" || len(b.Labels) != 2 {
		t.Errorf("cmd %v labels %v", b.Cmd, b.Labels)
	}
	for _, bad := range []string{"0.0.0.0", "8.8.8.8", "127.0.0.1", "::", "fd7a:115c:a1e0::1"} {
		if _, err := RelaySpec("sha256:abc", ctl, []netip.Addr{netip.MustParseAddr(bad)}, 7022); err == nil {
			t.Errorf("published on %s", bad)
		}
	}
	if _, err := RelaySpec("sha256:abc", ctl, nil, 7022); err == nil {
		t.Error("no addresses")
	}
	if _, err := RelaySpec("sha256:abc", ctl, addrs, 0); err == nil {
		t.Error("port 0")
	}
}

func TestChooseDirectAddrs(t *testing.T) {
	pf := func(s ...string) []netip.Prefix {
		var out []netip.Prefix
		for _, x := range s {
			out = append(out, netip.MustParsePrefix(x))
		}
		return out
	}
	got := ChooseDirectAddrs([]Iface{
		{Name: "lo", Up: true, Loopback: true, Addrs: pf("127.0.0.1/8")},
		{Name: "eth0", Up: true, Addrs: pf("192.168.1.20/24", "fe80::1/64", "2001:db8::5/64")},
		{Name: "eth1", Up: true, Addrs: pf("203.0.113.5/24")},
		{Name: "wlan0", Up: false, Addrs: pf("192.168.2.5/24")},
		{Name: "docker0", Up: true, Addrs: pf("172.17.0.1/16")},
		{Name: "br-1234", Up: true, Addrs: pf("172.18.0.1/16")},
		{Name: "cxabc123", Up: true, Addrs: pf("10.250.0.1/28")},
		{Name: "veth99", Up: true, Addrs: pf("10.9.9.9/24")},
		{Name: "tailscale0", Up: true, Addrs: pf("100.124.24.58/32", "fd7a:115c:a1e0::58/128")},
		{Name: "enp3s0", Up: true, Addrs: pf("10.0.0.7/8")},
		// Review D-L1: carrier-grade NAT on a WAN, a Docker network in
		// 100.64/10, and VPN tunnels aren't the LAN or the tailnet.
		{Name: "wan0", Up: true, Addrs: pf("100.72.1.9/10")},
		{Name: "br-cgnat", Up: true, Addrs: pf("100.80.0.1/16")},
		{Name: "wg0", Up: true, Addrs: pf("10.66.0.2/24")},
		{Name: "tun0", Up: true, Addrs: pf("172.27.0.6/24")},
		{Name: "zt5u4abc", Up: true, Addrs: pf("192.168.192.4/24")},
		{Name: "wt0", Up: true, Addrs: pf("100.90.1.2/16")},
		{Name: "tailscale1", Up: true, Addrs: pf("100.101.0.1/32")},
	})
	want := "100.101.0.1 100.124.24.58 10.0.0.7 192.168.1.20"
	var s []string
	for _, a := range got {
		s = append(s, a.String())
	}
	if strings.Join(s, " ") != want {
		t.Fatalf("got %v, want %s", s, want)
	}
}

// ---------------------------------------------------------------- the listener, end to end

type directRig struct {
	d       *Direct
	addr    string // the listener
	pub     ed25519.PublicKey
	machine string // the machine's port
	reports chan DirectReport
	now     time.Time
}

// newDirectRig runs the listener on 127.0.0.1 with relay as the sidecar's
// address, and a "machine" that echoes on another local port.
func newDirectRig(t *testing.T, relay string) *directRig {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(nil)
	now := time.Now()
	d := testDirect(now, 0)
	d.Now = time.Now
	d.Key = key
	cert, err := direct.NodeCert(key)
	if err != nil {
		t.Fatal(err)
	}
	d.cert = &cert
	d.relay, d.relayAt = netip.MustParseAddr(relay), time.Now().Add(time.Hour)
	d.gateway = netip.MustParseAddr("172.18.0.1")
	d.Pool = netip.MustParsePrefix("10.250.0.0/16")
	m, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := m.Accept()
			if err != nil {
				return
			}
			go func() { io.WriteString(c, "SSH-2.0-test\r\n"); io.Copy(c, c); c.Close() }()
		}
	}()
	mp := m.Addr().(*net.TCPAddr).Port
	reports := make(chan DirectReport, 100)
	d.Send = func(v any) {
		if r, ok := v.(DirectReport); ok {
			reports <- r
		}
	}
	d.Target = func(_ context.Context, sb string) (Target, bool, error) {
		return Target{Container: sb, Address: "127.0.0.1"}, sb == "cx-abc123", nil
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d.ln = ln
	go d.serve(ln)
	t.Cleanup(func() { ln.Close(); m.Close() })
	return &directRig{d: d, addr: ln.Addr().String(), pub: pub, machine: fmt.Sprint(mp), reports: reports, now: now}
}

func (r *directRig) ticket(sandbox string, port int) string {
	return r.ticketOf("", sandbox, port)
}

// ticketOf is a ticket asked for by token ("" for the admin key).
func (r *directRig) ticketOf(token, sandbox string, port int) string {
	c := claims(time.Now().Add(time.Minute))
	c.Sandbox, c.Port, c.Token = sandbox, port, token
	return direct.Mint(testSecret, c)
}

// connect sends the relay header for client, then the handshake and ticket.
func (r *directRig) connect(t *testing.T, client, ticket string) (net.Conn, error) {
	t.Helper()
	raw, err := net.Dial("tcp4", r.addr)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(raw, "CXR1 "+client+" 50000\n")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := direct.Handshake(ctx, raw, r.pub, ticket)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func TestListenerSession(t *testing.T) {
	r := newDirectRig(t, "127.0.0.1")
	var port int
	fmt.Sscan(r.machine, &port)
	tk := r.ticket("cx-abc123", port)
	c, err := r.connect(t, "192.168.65.1", tk)
	if err != nil {
		t.Fatal(err)
	}
	banner, _ := direct.ReadLine(c, 100)
	if banner != "SSH-2.0-test\r" {
		t.Fatalf("banner %q", banner)
	}
	io.WriteString(c, "ping\n")
	if l, _ := direct.ReadLine(c, 100); l != "ping" {
		t.Fatalf("echo %q", l)
	}
	if rep := <-r.reports; len(rep.Open) != 1 || !rep.Active || rep.Sandbox != "cx-abc123" {
		t.Fatalf("open report %+v", rep)
	}
	// The same ticket again.
	if _, err := r.connect(t, "192.168.65.1", tk); err == nil || !strings.Contains(err.Error(), "used already") {
		t.Fatalf("a replay: %v", err)
	}
	// Activity: the 30 s report says active after client bytes, and the
	// quiet report waits 5 minutes.
	r.d.reportDue()
	if rep := <-r.reports; !rep.Active {
		t.Fatalf("after bytes: %+v", rep)
	}
	r.d.reportDue()
	select {
	case rep := <-r.reports:
		t.Fatalf("a report while quiet: %+v", rep)
	default:
	}
	c.Close()
	select {
	case rep := <-r.reports:
		if len(rep.Open) != 0 {
			t.Fatalf("close report %+v", rep)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no report at close")
	}
}

func TestListenerRefusals(t *testing.T) {
	r := newDirectRig(t, "127.0.0.1")
	var port int
	fmt.Sscan(r.machine, &port)
	for name, c := range map[string]struct{ client, ticket, want string }{
		"not running":       {"192.168.65.1", r.ticket("cx-stoppd", port), "isn't running"},
		"nothing listening": {"192.168.65.1", r.ticket("cx-abc123", 1), "nothing answers"},
		"no ticket":         {"192.168.65.1", "", "malformed ticket"},
	} {
		_, err := r.connect(t, c.client, c.ticket)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// Clients the relay names that aren't allowed get nothing past the header.
	for _, client := range []string{"10.250.0.3", "8.8.8.8"} {
		if _, err := r.connect(t, client, r.ticket("cx-abc123", port)); err == nil {
			t.Errorf("client %s connected", client)
		}
	}
	// The agent network's gateway is how Tailscale's SNAT shows a tailnet
	// client: it gets a session with a good ticket.
	if c, err := r.connect(t, "172.18.0.1", r.ticket("cx-abc123", port)); err != nil {
		t.Errorf("a client at the gateway: %v", err)
	} else {
		c.Close()
	}
	// A line over 1 KiB.
	raw, _ := net.Dial("tcp4", r.addr)
	io.WriteString(raw, "CXR1 192.168.65.1 50000\n")
	tc, err := direct.HandshakeTLS(context.Background(), raw, r.pub)
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(tc, "CXD1 "+strings.Repeat("A", 2000)+"\n")
	if l, _ := direct.ReadLine(tc, 100); l != "err line too long" {
		t.Fatalf("a long line: %q", l)
	}
}

func TestListenerOnlyFromRelay(t *testing.T) {
	r := newDirectRig(t, "10.9.9.9") // the relay is elsewhere: 127.0.0.1 isn't it
	raw, _ := net.Dial("tcp4", r.addr)
	defer raw.Close()
	io.WriteString(raw, "CXR1 192.168.65.1 50000\n")
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := raw.Read(make([]byte, 10))
	if n != 0 || err == nil || isTimeout(err) {
		t.Fatalf("read %d %v; want closed at once", n, err)
	}
}

func TestListenerHandshakeLimits(t *testing.T) {
	r := newDirectRig(t, "127.0.0.1")
	// Connections that send the header and then nothing hold a handshake each.
	hold := func(client string) net.Conn {
		c, err := net.Dial("tcp4", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(c, "CXR1 "+client+" 50000\n")
		return c
	}
	closedAtOnce := func(c net.Conn) bool {
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, err := c.Read(make([]byte, 1))
		return err != nil && !isTimeout(err)
	}
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < DirectPerClientMax; i++ {
		held = append(held, hold("192.168.65.1"))
	}
	waitFor(t, func() bool {
		r.d.mu.Lock()
		defer r.d.mu.Unlock()
		return r.d.perClient[netip.MustParseAddr("192.168.65.1")] == DirectPerClientMax
	})
	if c := hold("192.168.65.1"); !closedAtOnce(c) {
		t.Fatal("the 17th handshake from one client wasn't closed")
	}
	// Other clients up to the node's cap, then the next is closed at accept.
	for i := DirectPerClientMax; i < DirectHandshakesMax; i++ {
		held = append(held, hold(fmt.Sprintf("192.168.66.%d", i)))
	}
	waitFor(t, func() bool { r.d.mu.Lock(); defer r.d.mu.Unlock(); return r.d.hs == DirectHandshakesMax })
	c, _ := net.Dial("tcp4", r.addr)
	if !closedAtOnce(c) {
		t.Fatal("the handshake over the node's cap wasn't closed")
	}
	// The deadline frees them.
	for _, c := range held {
		c.Close()
	}
	held = nil
	waitFor(t, func() bool { r.d.mu.Lock(); defer r.d.mu.Unlock(); return r.d.hs == 0 && len(r.d.perClient) == 0 })
}

func TestListenerSessionCaps(t *testing.T) {
	r := newDirectRig(t, "127.0.0.1")
	var port int
	fmt.Sscan(r.machine, &port)
	var mu sync.Mutex
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < direct.SessionsMax; i++ {
		c, err := r.connect(t, fmt.Sprintf("192.168.%d.%d", 70+i/200, i%200+1), r.ticket("cx-abc123", port))
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		mu.Lock()
		conns = append(conns, c)
		mu.Unlock()
	}
	if _, err := r.connect(t, "192.168.99.1", r.ticket("cx-abc123", port)); err == nil || !strings.Contains(err.Error(), "direct sessions open") {
		t.Fatalf("over the sandbox's cap: %v", err)
	}
}

// The relay names the client, then carries both ways, half-close included.
func TestRelay(t *testing.T) {
	agent, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close()
	front, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Relay(ctx, front, agent.Addr().String(), nil)
	c, err := net.Dial("tcp4", front.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.Accept()
	if err != nil {
		t.Fatal(err)
	}
	line, err := direct.ReadLine(a, direct.RelayHeaderMax)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := direct.ParseRelayHeader(line)
	if err != nil || cl.String() != c.LocalAddr().String() {
		t.Fatalf("header %q names %v, not the client %v (%v)", line, cl, c.LocalAddr(), err)
	}
	io.WriteString(c, "hi")
	c.(*net.TCPConn).CloseWrite()
	if b, _ := io.ReadAll(a); string(b) != "hi" {
		t.Fatalf("agent read %q", b)
	}
	io.WriteString(a, "back")
	a.Close()
	if b, _ := io.ReadAll(c); string(b) != "back" {
		t.Fatalf("client read %q", b)
	}
}

// Review D-M1: the agent ends a token's sessions on its own evidence (the
// token in each ticket), whatever the Worker remembers of the session ids,
// refuses the token's unused tickets, names the tokens in its reports, and
// reports every open session again after a welcome, so a lost open report
// is made good.
func TestEndByToken(t *testing.T) {
	r := newDirectRig(t, "127.0.0.1")
	var port int
	fmt.Sscan(r.machine, &port)
	drain := func() []DirectReport {
		var out []DirectReport
		for {
			select {
			case rep := <-r.reports:
				out = append(out, rep)
			case <-time.After(200 * time.Millisecond):
				return out
			}
		}
	}
	open := func(token string) net.Conn {
		c, err := r.connect(t, "192.168.65.1", r.ticketOf(token, "cx-abc123", port))
		if err != nil {
			t.Fatalf("%q: %v", token, err)
		}
		direct.ReadLine(c, 100)
		return c
	}
	revoked, kept, admin := open("tk-revoked1"), open("tk-keepme12"), open("")
	defer kept.Close()
	defer admin.Close()
	reps := drain()
	if last := reps[len(reps)-1]; len(last.Open) != 3 || strings.Join(last.Tokens, ",") != "tk-keepme12,tk-revoked1" {
		t.Fatalf("the report names the tokens of the open sessions, not the admin key's: %+v", last)
	}
	// A welcome again (the control socket reconnected): every open session is reported.
	r.d.Welcome(&WelcomeDirect{Secret: base64.StdEncoding.EncodeToString(testSecret), Now: time.Now().UnixMilli()})
	if reps := drain(); len(reps) != 1 || len(reps[0].Open) != 3 || len(reps[0].Tokens) != 2 {
		t.Fatalf("after a welcome: %+v", reps)
	}
	// The Worker names only the token, no session id.
	if n := r.d.End(DirectEnd{Sandbox: "cx-abc123", Sessions: []string{}, Tokens: []string{"tk-revoked1"}}); n != 1 {
		t.Fatalf("ended %d sessions; want the revoked token's one", n)
	}
	revoked.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadAll(revoked); isTimeout(err) {
		t.Fatal("the revoked token's session is still open")
	}
	io.WriteString(kept, "still\n")
	if l, _ := direct.ReadLine(kept, 100); l != "still" {
		t.Fatalf("another token's session: %q", l)
	}
	if _, err := r.connect(t, "192.168.65.1", r.ticketOf("tk-revoked1", "cx-abc123", port)); err == nil || !strings.Contains(err.Error(), "its token ended") {
		t.Fatalf("an unused ticket of the ended token: %v", err)
	}
	// Another sandbox's directEnd doesn't touch these.
	if n := r.d.End(DirectEnd{Sandbox: "cx-other1", Sessions: []string{}, Tokens: []string{"tk-keepme12"}}); n != 0 {
		t.Fatalf("another sandbox's directEnd closed %d", n)
	}
}

// Review D-L2: the agent network's gateway (every tailnet client behind
// Tailscale's SNAT) has a budget of its own, larger than one LAN address's
// and leaving the rest of the node's to the LAN; one token can't take the
// node's seen set or its sessions.
func TestDirectSharesOfTheLimits(t *testing.T) {
	r := newDirectRig(t, "127.0.0.1")
	hold := func(client string) net.Conn {
		c, err := net.Dial("tcp4", r.addr)
		if err != nil {
			t.Fatal(err)
		}
		io.WriteString(c, "CXR1 "+client+" 50000\n")
		return c
	}
	var held []net.Conn
	defer func() {
		for _, c := range held {
			c.Close()
		}
	}()
	for i := 0; i < DirectGatewayMax; i++ {
		held = append(held, hold("172.18.0.1"))
	}
	waitFor(t, func() bool {
		r.d.mu.Lock()
		defer r.d.mu.Unlock()
		return r.d.perClient[netip.MustParseAddr("172.18.0.1")] == DirectGatewayMax
	})
	c := hold("172.18.0.1")
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); err == nil || isTimeout(err) {
		t.Fatal("a handshake over the gateway's budget wasn't closed")
	}
	// A LAN client still gets in.
	var port int
	fmt.Sscan(r.machine, &port)
	s, err := r.connect(t, "192.168.65.1", r.ticket("cx-abc123", port))
	if err != nil {
		t.Fatalf("a LAN client with the gateway's budget used: %v", err)
	}
	s.Close()

	// One token's seen tickets.
	now := time.Unix(1_800_000_000, 0)
	d := testDirect(now, 0)
	tok := func(token string) string {
		c := claims(now.Add(time.Minute))
		c.Token = token
		return direct.Mint(testSecret, c)
	}
	for i := 0; i < DirectTokenSeenMax; i++ {
		if _, why := d.admit(tok("tk-greedy01")); why != "" {
			t.Fatalf("ticket %d: %s", i, why)
		}
	}
	if _, why := d.admit(tok("tk-greedy01")); !strings.Contains(why, "of this token") {
		t.Fatalf("over the token's share: %q", why)
	}
	if _, why := d.admit(tok("tk-other001")); why != "" {
		t.Fatalf("another token: %q", why)
	}
	if _, why := d.admit(tok("")); why != "" {
		t.Fatalf("the admin key: %q", why)
	}
	// Forgotten with the tickets, counts and all.
	d.Now = func() time.Time { return now.Add(time.Minute + DirectGrace + time.Second) }
	d.mu.Lock()
	d.pruneSeenLocked(d.now())
	left := len(d.seenBy)
	d.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d token counts left after their tickets expired", left)
	}

	// One token's open sessions on the node, across sandboxes.
	d2 := testDirect(now, 0)
	a, b := net.Pipe()
	defer b.Close()
	d2.mu.Lock()
	for i := 0; i < DirectTokenSessionsMax; i++ {
		d2.sessions[fmt.Sprintf("%016x", i)] = &directSession{id: fmt.Sprintf("%016x", i), sandbox: fmt.Sprintf("cx-sb%04d", i), token: "tk-greedy01", conn: a}
	}
	d2.mu.Unlock()
	c2 := claims(now.Add(time.Minute))
	c2.Sandbox, c2.Token = "cx-new001", "tk-greedy01"
	if _, why := d2.open(c2, a); !strings.Contains(why, "this token has") {
		t.Fatalf("over the token's sessions: %q", why)
	}
	c2.Token = "tk-other001"
	if _, why := d2.open(c2, a); why != "" {
		t.Fatalf("another token: %q", why)
	}
}

// Re-review 1: a directEnd for the token between admit and open (the
// machine's lookup and dial) must still keep the session from opening.
func TestEndBetweenAdmitAndOpen(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	d := testDirect(now, 0)
	c := claims(now.Add(60 * time.Second))
	c.Token = "tk-aaaaaaaa"
	cl, why := d.admit(direct.Mint(testSecret, c))
	if why != "" {
		t.Fatal(why)
	}
	d.End(DirectEnd{Sandbox: c.Sandbox, Tokens: []string{"tk-aaaaaaaa"}})
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if s, why := d.open(cl, a); s != nil || why != "its token ended" {
		t.Fatalf("opened %v, %q", s != nil, why)
	}
	// Another token's ticket admitted at the same time still opens.
	c2 := claims(now.Add(60 * time.Second))
	c2.Token = "tk-bbbbbbbb"
	cl2, _ := d.admit(direct.Mint(testSecret, c2))
	if s, why := d.open(cl2, a); s == nil {
		t.Fatalf("another token: %q", why)
	}
}
