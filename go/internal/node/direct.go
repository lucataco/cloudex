package node

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lucataco/cloudex/go/internal/direct"
)

// The direct path (v0.2.3; packages/schema/src/node.ts, "the direct path";
// research/v0.2.3-plan.md M2): ssh and other TCP connections from a client
// on the LAN or a tailnet straight to a machine here, without the Worker.
//
//	client ──TCP──▶ host addr:port ──Docker publish──▶ cloudex-node-direct (relay.go)
//	        ──"CXR1 <client>"──▶ this listener, on the agent's control address only
//	        ──TLS 1.3 (the node key), "CXD1 <ticket>"──▶ "ok" ──▶ the machine's port
//
// The listener takes a TCP connection only from the sidecar's address, and a
// client only from a private or tailnet address outside the machine pool. The
// agent network's gateway is allowed: Tailscale's default SNAT (NoSNAT false)
// masquerades every tailnet client of a Docker publish as it (v0.2.3 M4, on
// cataco), and so does a machine's own network for a machine without the
// firewall (M0 Q3), which the firewall keeps from the publish. A ticket is the Worker's
// (go/internal/direct): its MAC under the secret that came in welcome, this
// node, not expired by the Worker's clock, not seen before, a machine here
// that runs. Sessions are reported to the Worker so that they count as
// activity and end with their token (DirectReport, the directEnd op).

// Limits on the direct port (plan M2, "Limits").
const (
	DirectHandshakesMax   = 64   // TLS handshakes and ticket lines at once
	DirectPerClientMax    = 16   // of those from one client address
	DirectSessionsMax     = 256  // open sessions on the node
	DirectSeenMax         = 4096 // session ids remembered until their ticket expires
	DirectEndedTokensMax  = 1024 // tokens that ended, remembered until their tickets expire
	DirectGrace           = 10 * time.Second
	DirectQuietReport     = 5 * time.Minute // a report while sessions are open but quiet
	DirectSidecarEvery    = 30 * time.Second
	directRelayRecheckGap = 2 * time.Second
)

// directEndMax bounds a directEnd's list: the most sessions a Sandbox object
// keeps (direct.ts SESSIONS_KEPT).
const directEndMax = 4 * direct.SessionsMax

// DirectConfig is the direct port as the installer chose it.
type DirectConfig struct {
	Port  int          // the host port Docker publishes; 0: no direct path
	Addrs []netip.Addr // the host addresses it's published on
}

// Direct is the agent's direct listener and its sidecar.
type Direct struct {
	Config DirectConfig
	NodeID string
	Key    ed25519.PrivateKey
	Docker *Docker
	Self   string       // the agent's own container
	Pool   netip.Prefix // the machine pool: no client comes from it
	Log    *log.Logger
	Debug  bool
	// Target finds a running machine; Send sends a message on the control
	// socket (dropped while it's down); Changed says the facts changed.
	Target  func(ctx context.Context, sandbox string) (Target, bool, error)
	Send    func(v any)
	Changed func()
	// Now is the local clock (tests).
	Now func() time.Time
	// OnListening is called when the listener first opens, so that a
	// self-test probes it.
	OnListening func()

	mu        sync.Mutex
	secret    []byte
	skew      time.Duration // the Worker's clock less ours, from welcome
	welcomed  bool          // a welcome came; secret nil then means an older Worker
	revoked   bool
	ln        net.Listener
	ctl       netip.Addr // the agent's address on its network
	gateway   netip.Addr // that network's gateway
	relay     netip.Addr // the sidecar's address
	relayAt   time.Time  // when relay was last read from Docker
	state     string     // listening, off or failed
	why       string
	addrs     []string // the sidecar's actual bindings
	public    bool     // a binding isn't private or tailnet
	hs        int
	perClient map[netip.Addr]int
	sessions  map[string]*directSession
	seen      map[string]time.Time // session id -> forget after (local clock)
	ended     map[string]time.Time // token id -> refuse its tickets until (local clock)
	reported  map[string]time.Time // sandbox -> last report
	kick      chan struct{}
	listened  bool
	cert      *tls.Certificate
}

type directSession struct {
	id, sandbox string
	token       string // the token whose ticket opened it; "" for the admin key
	conn        net.Conn
	active      atomic.Bool // the client sent bytes since the last report
}

func (d *Direct) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Direct) logf(format string, args ...any) {
	if d.Log != nil {
		d.Log.Printf("direct: "+format, args...)
	}
}

func (d *Direct) init() {
	if d.perClient == nil {
		d.perClient = map[netip.Addr]int{}
		d.sessions = map[string]*directSession{}
		d.seen = map[string]time.Time{}
		d.ended = map[string]time.Time{}
		d.reported = map[string]time.Time{}
		d.kick = make(chan struct{}, 1)
		if d.state == "" {
			d.state = "off"
		}
	}
}

// Fact is the direct path for the node's facts; nil when it's turned off.
func (d *Direct) Fact() *DirectFact {
	if d == nil || d.Config.Port == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.init()
	f := &DirectFact{Port: d.Config.Port, Addrs: []string{}, State: d.state, Error: d.why}
	for _, a := range d.addrs {
		if p, err := netip.ParseAddr(a); err == nil && p.Is4() && len(f.Addrs) < DirectAddrsMax {
			f.Addrs = append(f.Addrs, a)
		}
	}
	if len(f.Error) > 300 {
		f.Error = f.Error[:300]
	}
	return f
}

// setState changes the state, and says so when it changed.
func (d *Direct) setStateLocked(state, why string) bool {
	if d.state == state && d.why == why {
		return false
	}
	if state == "failed" {
		d.logf("failed: %s", why)
	} else if d.state != state {
		if why == "" {
			d.logf("%s", state)
		} else {
			d.logf("%s: %s", state, why)
		}
	}
	d.state, d.why = state, why
	return true
}

func (d *Direct) changed() {
	if d.Changed != nil {
		d.Changed()
	}
}

// Welcome takes welcome's direct part: the secret and the Worker's clock.
// None (a Worker older than v0.2.3) means the direct path stays off.
func (d *Direct) Welcome(w *WelcomeDirect) {
	if d == nil || d.Config.Port == 0 {
		return
	}
	d.mu.Lock()
	d.init()
	d.welcomed = true
	if w == nil {
		d.secret = nil
		d.mu.Unlock()
		d.closeAll("the deployment is older than v0.2.3")
		d.wake()
		return
	}
	secret, err := direct.DecodeSecret(w.Secret)
	if err != nil {
		d.secret = nil
		d.logf("welcome: %v", err)
	} else {
		d.secret = secret
		if w.Now > 0 {
			d.skew = time.Duration(w.Now-d.now().UnixMilli()) * time.Millisecond
		}
	}
	d.revoked = false
	have := d.secret != nil
	d.mu.Unlock()
	d.wake()
	// Every sandbox's open sessions again, with their tokens: a report lost
	// while the link was down (or over the node's report budget) would
	// otherwise wait for the next quiet report (review D-M1).
	if have {
		go d.reportAll()
	}
}

func (d *Direct) wake() {
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// Revoke is the node's removal (R3): the listener and every session close,
// the secret is forgotten and the sidecar removed.
func (d *Direct) Revoke(ctx context.Context) {
	if d == nil || d.Config.Port == 0 {
		return
	}
	d.mu.Lock()
	d.init()
	d.revoked, d.secret = true, nil
	d.mu.Unlock()
	d.closeAll("this node was removed from the deployment")
	if err := d.removeSidecar(ctx); err != nil {
		d.logf("removing the sidecar: %v", err)
	}
	d.mu.Lock()
	d.addrs = nil
	ch := d.setStateLocked("off", "this node was removed from the deployment")
	d.mu.Unlock()
	if ch {
		d.changed()
	}
}

// closeAll closes the listener and every session.
func (d *Direct) closeAll(why string) {
	d.mu.Lock()
	ln := d.ln
	d.ln = nil
	var ss []*directSession
	for _, s := range d.sessions {
		ss = append(ss, s)
	}
	ch := d.setStateLocked("off", why)
	d.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	for _, s := range ss {
		s.conn.Close()
	}
	if ch {
		d.changed()
	}
}

// Run keeps the sidecar up, the listener open while the deployment has sent
// a secret, and the reports going, until ctx ends.
func (d *Direct) Run(ctx context.Context) {
	if d.Config.Port == 0 {
		return
	}
	d.mu.Lock()
	d.init()
	d.mu.Unlock()
	if d.cert == nil {
		cert, err := direct.NodeCert(d.Key)
		if err != nil {
			d.mu.Lock()
			d.setStateLocked("failed", "the node certificate: "+err.Error())
			d.mu.Unlock()
			return
		}
		d.cert = &cert
	}
	go d.reports(ctx)
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			d.closeAll("the agent stopped")
			return
		case <-t.C:
		case <-d.kick:
		}
		d.tick(ctx)
		t.Reset(DirectSidecarEvery)
	}
}

func (d *Direct) tick(ctx context.Context) {
	d.mu.Lock()
	revoked, haveSecret, welcomed := d.revoked, d.secret != nil, d.welcomed
	d.mu.Unlock()
	if revoked {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err := d.ensureSidecar(cctx)
	d.mu.Lock()
	var ch bool
	switch {
	case err != nil:
		ch = d.setStateLocked("failed", err.Error())
	case !haveSecret:
		why := "waiting for the deployment's secret"
		if welcomed {
			why = "the deployment sent no direct secret (older than v0.2.3)"
		}
		ch = d.setStateLocked("off", why)
	}
	ln := d.ln
	d.mu.Unlock()
	if err != nil || !haveSecret {
		if ln != nil {
			d.closeListener()
		}
		if ch {
			d.changed()
		}
		return
	}
	if ln == nil {
		if lerr := d.listen(); lerr != nil {
			d.mu.Lock()
			ch = d.setStateLocked("failed", lerr.Error()) || ch
			d.mu.Unlock()
			if ch {
				d.changed()
			}
			return
		}
	}
	d.mu.Lock()
	ch = d.setStateLocked("listening", "") || ch
	first := !d.listened
	d.listened = true
	d.mu.Unlock()
	if ch {
		d.changed()
	}
	if first && d.OnListening != nil {
		d.OnListening()
	}
}

func (d *Direct) closeListener() {
	d.mu.Lock()
	ln := d.ln
	d.ln = nil
	d.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
}

// listen opens the listener on the agent's control address, bound to its
// interface: never 0.0.0.0 or a machine network.
func (d *Direct) listen() error {
	d.mu.Lock()
	ctl := d.ctl
	d.mu.Unlock()
	if !ctl.IsValid() {
		return errors.New("the agent has no address on its network " + AgentNetwork)
	}
	ifname := interfaceOf(ctl)
	ln, err := listenOn(netip.AddrPortFrom(ctl, RelayPort).String(), ifname)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", netip.AddrPortFrom(ctl, RelayPort), err)
	}
	d.mu.Lock()
	if d.ln != nil {
		d.mu.Unlock()
		ln.Close()
		return nil
	}
	d.ln = ln
	d.mu.Unlock()
	d.logf("listening on %s (interface %q) for the relay", ln.Addr(), ifname)
	go d.serve(ln)
	return nil
}

func (d *Direct) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			d.mu.Lock()
			mine := d.ln == ln
			d.mu.Unlock()
			if mine && !errors.Is(err, net.ErrClosed) {
				d.logf("accept: %v", err)
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return
		}
		d.accept(c)
	}
}

// accept takes or closes a connection at once; the rest runs on its own.
func (d *Direct) accept(c net.Conn) {
	peer, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil || !d.fromRelay(peer.Addr().Unmap()) {
		d.debugf("closed a connection from %s: not the relay", c.RemoteAddr())
		c.Close()
		return
	}
	d.mu.Lock()
	if d.hs >= DirectHandshakesMax {
		d.mu.Unlock()
		c.Close()
		return
	}
	d.hs++
	d.mu.Unlock()
	go func() {
		defer func() {
			if p := recover(); p != nil {
				d.logf("a connection panicked: %v", p)
				c.Close()
			}
		}()
		d.handle(c)
	}()
}

func (d *Direct) debugf(format string, args ...any) {
	if d.Debug {
		d.logf(format, args...)
	}
}

// fromRelay says whether peer is the sidecar's address, reading it from
// Docker again (at most every few seconds) when it isn't: a restarted
// sidecar may have a new one.
func (d *Direct) fromRelay(peer netip.Addr) bool {
	d.mu.Lock()
	relay, at := d.relay, d.relayAt
	d.mu.Unlock()
	if relay.IsValid() && peer == relay {
		return true
	}
	if time.Since(at) < directRelayRecheckGap {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := d.Docker.ContainerInspect(ctx, RelayName)
	d.mu.Lock()
	d.relayAt = time.Now()
	d.mu.Unlock()
	if err != nil || !isRelay(c) || !c.State.Running {
		return false
	}
	a, err := netip.ParseAddr(c.NetworkSettings.Networks[AgentNetwork].IPAddress)
	if err != nil {
		return false
	}
	d.mu.Lock()
	d.relay = a
	d.mu.Unlock()
	return a == peer
}

// ClientAllowed is the rule for the client the relay names: private or
// tailnet IPv4, not in the machine pool. The agent network's gateway is
// allowed (see the top): a tailnet client behind Tailscale's SNAT shows as
// it, and can't be told from a machine that reaches the publish without the
// firewall (--unisolated), which still needs a ticket.
func ClientAllowed(client netip.Addr, pool netip.Prefix) error {
	client = client.Unmap()
	switch {
	case !direct.Usable(client):
		return errors.New("not a private or tailnet address")
	case pool.IsValid() && pool.Contains(client):
		return errors.New("inside the machine pool")
	}
	return nil
}

// handle runs one connection from the relay: the header, TLS, the ticket.
func (d *Direct) handle(c net.Conn) {
	inHS := true
	var client netip.Addr
	done := func() {
		if !inHS {
			return
		}
		inHS = false
		d.mu.Lock()
		d.hs--
		if client.IsValid() {
			if d.perClient[client]--; d.perClient[client] <= 0 {
				delete(d.perClient, client)
			}
		}
		d.mu.Unlock()
	}
	defer done()
	c.SetDeadline(d.now().Add(direct.HandshakeTimeout))
	line, err := direct.ReadLine(c, direct.RelayHeaderMax)
	if err != nil {
		c.Close()
		return
	}
	cl, err := direct.ParseRelayHeader(line)
	if err != nil {
		d.logf("closed a connection from the relay: %v", err)
		c.Close()
		return
	}
	d.mu.Lock()
	gw, pool := d.gateway, d.Pool
	d.mu.Unlock()
	if err := ClientAllowed(cl.Addr(), pool); err != nil {
		d.logf("refused %s: %v", cl, err)
		c.Close()
		return
	}
	if gw.IsValid() && cl.Addr() == gw {
		d.debugf("client %s is the agent network's gateway: behind the host's NAT (Tailscale's SNAT), or a machine without the firewall", cl)
	}
	d.mu.Lock()
	if d.perClient[cl.Addr()] >= DirectPerClientMax {
		d.mu.Unlock()
		d.debugf("refused %s: %d handshakes from it already", cl, DirectPerClientMax)
		c.Close()
		return
	}
	client = cl.Addr()
	d.perClient[client]++
	cert := d.cert
	d.mu.Unlock()

	tc := tls.Server(c, direct.ServerTLS(*cert))
	if err := tc.Handshake(); err != nil {
		d.debugf("%s: TLS: %v", cl, err)
		c.Close()
		return
	}
	if tc.ConnectionState().NegotiatedProtocol != direct.ALPN {
		d.debugf("%s: no ALPN %s", cl, direct.ALPN)
		tc.Close()
		return
	}
	refuse := func(why string) {
		d.logf("refused a ticket from %s: %s", cl, why)
		tc.SetWriteDeadline(d.now().Add(time.Second))
		io.WriteString(tc, "err "+why+"\n")
		tc.Close()
	}
	line, err = direct.ReadLine(tc, direct.LineMax)
	if errors.Is(err, direct.ErrLineTooLong) {
		refuse("line too long")
		return
	}
	if err != nil {
		tc.Close()
		return
	}
	ticket, ok := strings.CutPrefix(line, "CXD1 ")
	if !ok {
		refuse("expected CXD1 <ticket>")
		return
	}
	claims, why := d.admit(ticket)
	if why != "" {
		refuse(why)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), direct.HandshakeTimeout)
	m, running, err := d.Target(ctx, claims.Sandbox)
	cancel()
	if err != nil || !running {
		refuse(claims.Sandbox + " isn't running on this node")
		return
	}
	dctx, dcancel := context.WithTimeout(context.Background(), direct.HandshakeTimeout)
	mc, err := dialMachine(dctx, m, claims.Port)
	dcancel()
	if err != nil {
		refuse(fmt.Sprintf("nothing answers on port %d of %s", claims.Port, claims.Sandbox))
		return
	}
	s, why := d.open(claims, tc)
	if why != "" {
		mc.Close()
		refuse(why)
		return
	}
	defer d.closeSession(s)
	if _, err := io.WriteString(tc, "ok\n"); err != nil {
		mc.Close()
		tc.Close()
		return
	}
	tc.SetDeadline(time.Time{})
	done()
	d.logf("%s: session %s to %s:%d", cl, claims.Session, claims.Sandbox, claims.Port)
	d.report(claims.Sandbox, true)
	direct.Splice(tc, mc, func(int) { s.active.Store(true) })
	d.debugf("%s: session %s ended", cl, claims.Session)
}

// admit checks a ticket and marks its session seen; why is the refusal.
func (d *Direct) admit(ticket string) (direct.Claims, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.secret == nil {
		return direct.Claims{}, "the node has no secret from the deployment yet"
	}
	c, err := direct.Verify(d.secret, ticket)
	if err != nil {
		return c, err.Error()
	}
	now := d.now()
	if c.Node != d.NodeID {
		return c, "a ticket for another node"
	}
	if c.Expired(now.Add(d.skew), DirectGrace) {
		return c, "expired"
	}
	d.pruneSeenLocked(now)
	if _, ok := d.seen[c.Session]; ok {
		return c, "used already"
	}
	if c.Token != "" {
		if _, ok := d.ended[c.Token]; ok {
			return c, "its token ended"
		}
	}
	if len(d.seen) >= DirectSeenMax {
		return c, "busy: too many tickets in the last minute"
	}
	d.seen[c.Session] = time.Unix(c.Exp, 0).Add(-d.skew).Add(DirectGrace)
	return c, ""
}

func (d *Direct) pruneSeenLocked(now time.Time) {
	for id, until := range d.seen {
		if now.After(until) {
			delete(d.seen, id)
		}
	}
	for t, until := range d.ended {
		if now.After(until) {
			delete(d.ended, t)
		}
	}
}

// open registers a session within the caps.
func (d *Direct) open(c direct.Claims, conn net.Conn) (*directSession, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.secret == nil {
		return nil, "the direct path closed"
	}
	if len(d.sessions) >= DirectSessionsMax {
		return nil, fmt.Sprintf("this node has %d direct sessions open", DirectSessionsMax)
	}
	n := 0
	for _, s := range d.sessions {
		if s.sandbox == c.Sandbox {
			n++
		}
	}
	if n >= direct.SessionsMax {
		return nil, fmt.Sprintf("%s has %d direct sessions open", c.Sandbox, direct.SessionsMax)
	}
	s := &directSession{id: c.Session, sandbox: c.Sandbox, token: c.Token, conn: conn}
	d.sessions[s.id] = s
	return s, ""
}

func (d *Direct) closeSession(s *directSession) {
	d.mu.Lock()
	_, was := d.sessions[s.id]
	delete(d.sessions, s.id)
	d.mu.Unlock()
	s.conn.Close()
	if was {
		d.report(s.sandbox, false)
	}
}

// openOf lists a sandbox's open sessions.
func (d *Direct) openOfLocked(sandbox string) []string {
	var ids []string
	for _, s := range d.sessions {
		if s.sandbox == sandbox {
			ids = append(ids, s.id)
		}
	}
	sort.Strings(ids)
	if len(ids) > direct.SessionsMax {
		ids = ids[:direct.SessionsMax]
	}
	return ids
}

// report sends a sandbox's open sessions now, if the deployment sent a
// secret (an older Worker closes the socket on a message it doesn't know).
func (d *Direct) report(sandbox string, active bool) {
	d.mu.Lock()
	if d.secret == nil || d.Send == nil {
		d.mu.Unlock()
		return
	}
	open := d.openOfLocked(sandbox)
	var tokens []string
	for _, s := range d.sessions {
		if s.sandbox != sandbox {
			continue
		}
		if s.active.Swap(false) {
			active = true
		}
		if s.token != "" && !slices.Contains(tokens, s.token) && len(tokens) < direct.SessionsMax {
			tokens = append(tokens, s.token)
		}
	}
	sort.Strings(tokens)
	d.reported[sandbox] = d.now()
	if len(open) == 0 {
		delete(d.reported, sandbox)
	}
	send := d.Send
	d.mu.Unlock()
	send(NewDirectReport(sandbox, open, active, tokens...))
}

// reportAll sends a report for every sandbox with sessions open.
func (d *Direct) reportAll() {
	d.mu.Lock()
	sbs := map[string]bool{}
	for _, s := range d.sessions {
		sbs[s.sandbox] = true
	}
	d.mu.Unlock()
	for sb := range sbs {
		d.report(sb, false)
	}
}

// reports sends, every direct.ReportEvery, a report for each sandbox whose
// clients sent bytes, and one every DirectQuietReport for those with
// sessions open but quiet (plan M2, after M1's rate advice).
func (d *Direct) reports(ctx context.Context) {
	t := time.NewTicker(direct.ReportEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.reportDue()
	}
}

func (d *Direct) reportDue() {
	d.mu.Lock()
	now := d.now()
	due := map[string]bool{}
	for _, s := range d.sessions {
		last, ok := d.reported[s.sandbox]
		if s.active.Load() || !ok || now.Sub(last) >= DirectQuietReport {
			due[s.sandbox] = true
		}
	}
	d.mu.Unlock()
	for sb := range due {
		d.report(sb, false)
	}
}

// End is the directEnd op: it closes the named sessions of the sandbox, and
// every session of the sandbox opened with a ticket of one of the named
// tokens, and refuses their tickets until they expire (a token's, anywhere
// on the node: a token that ended doesn't come back); it answers how many it
// closed.
func (d *Direct) End(e DirectEnd) int {
	if d == nil {
		return 0
	}
	d.mu.Lock()
	d.init()
	now := d.now()
	d.pruneSeenLocked(now)
	var closing []*directSession
	for _, id := range e.Sessions {
		if s, ok := d.sessions[id]; ok && s.sandbox == e.Sandbox {
			closing = append(closing, s)
		}
		if _, ok := d.seen[id]; !ok && len(d.seen) < 2*DirectSeenMax {
			d.seen[id] = now.Add(direct.TicketTTL + DirectGrace)
		}
	}
	for _, t := range e.Tokens {
		if t == "" {
			continue
		}
		for _, s := range d.sessions {
			if s.token == t && s.sandbox == e.Sandbox && !slices.Contains(closing, s) {
				closing = append(closing, s)
			}
		}
		if len(d.ended) < DirectEndedTokensMax {
			d.ended[t] = now.Add(direct.TicketTTL + DirectGrace)
		}
	}
	d.mu.Unlock()
	for _, s := range closing {
		d.logf("ended session %s of %s: its token ended", s.id, s.sandbox)
		s.conn.Close()
	}
	return len(closing)
}

// endDirect is the directEnd op's answer.
func (a *Agent) endDirect(e DirectEnd) int {
	return a.Direct.End(e)
}

// ---------------------------------------------------------------- the sidecar

func isRelay(c *ContainerDetail) bool {
	return c != nil && c.Config.Labels[LabelAgent] == "1" && c.Config.Labels[LabelRole] == "direct"
}

// ensureSidecar makes sure the sidecar runs, on the agent's network, with
// its bindings live; it records its address and its bindings. A sidecar
// that isn't running is removed and made again, never started (M0 Q2: a
// container whose start failed has lost its network, and a later start runs
// it without one). A running one is left as it is, and its actual bindings
// are what is reported.
func (d *Direct) ensureSidecar(ctx context.Context) error {
	return d.sidecar(ctx, true)
}

// Observe reads the agent's address and the sidecar's, and its bindings,
// changing nothing (the self-test run by hand, outside the agent).
func (d *Direct) Observe(ctx context.Context) error {
	d.mu.Lock()
	d.init()
	d.mu.Unlock()
	err := d.sidecar(ctx, false)
	d.mu.Lock()
	if err != nil {
		d.setStateLocked("failed", err.Error())
	} else {
		d.setStateLocked("listening", "")
	}
	d.mu.Unlock()
	return err
}

func (d *Direct) sidecar(ctx context.Context, fix bool) error {
	self, err := d.Docker.ContainerInspect(ctx, d.Self)
	if err != nil {
		return fmt.Errorf("the agent's own container: %w", err)
	}
	n, ok := self.NetworkSettings.Networks[AgentNetwork]
	if !ok {
		return fmt.Errorf("the agent isn't on its network %s", AgentNetwork)
	}
	ctl, err := netip.ParseAddr(n.IPAddress)
	if err != nil || !ctl.Is4() {
		return fmt.Errorf("the agent has no IPv4 address on %s", AgentNetwork)
	}
	gw, _ := netip.ParseAddr(n.Gateway)
	d.mu.Lock()
	if fix && d.ctl != ctl && d.ln != nil {
		// The agent's address changed: listen again on the new one.
		d.ln.Close()
		d.ln = nil
	}
	d.ctl, d.gateway = ctl, gw
	d.mu.Unlock()

	c, err := d.Docker.ContainerInspect(ctx, RelayName)
	if err != nil && !IsNotFound(err) {
		return fmt.Errorf("the sidecar: %w", err)
	}
	if c != nil && !isRelay(c) {
		return fmt.Errorf("a container named %s that isn't the agent's is in the way", RelayName)
	}
	if !fix && (c == nil || !c.State.Running) {
		return fmt.Errorf("the sidecar %s isn't running", RelayName)
	}
	if fix && c != nil && c.State.Running && !slices.Equal(c.Config.Cmd, RelayCmd(ctl)) {
		d.logf("the sidecar relays to %v, not to %s: making it again", c.Config.Cmd, ctl)
		c.State.Running = false
	}
	if c == nil || !c.State.Running {
		if c != nil {
			if err := d.Docker.ContainerRemove(ctx, c.ID); err != nil {
				return fmt.Errorf("removing the stopped sidecar: %w", err)
			}
		}
		spec, err := RelaySpec(self.Image, ctl, d.Config.Addrs, d.Config.Port)
		if err != nil {
			return err
		}
		id, err := d.Docker.ContainerCreate(ctx, RelayName, spec)
		if err != nil {
			return fmt.Errorf("making the sidecar: %w", err)
		}
		if err := d.Docker.ContainerStart(ctx, id); err != nil {
			// Docker's reason names the address that's missing.
			d.Docker.ContainerRemove(ctx, id)
			return fmt.Errorf("starting the sidecar: %w", err)
		}
		d.logf("started the sidecar %s, publishing port %d on %v", RelayName, d.Config.Port, d.Config.Addrs)
		if c, err = d.Docker.ContainerInspect(ctx, id); err != nil {
			return fmt.Errorf("the sidecar: %w", err)
		}
	}
	relay, err := netip.ParseAddr(c.NetworkSettings.Networks[AgentNetwork].IPAddress)
	if err != nil {
		return fmt.Errorf("the sidecar has no address on %s; it is made again within %v", AgentNetwork, DirectSidecarEvery)
	}
	addrs, public := Bindings(c.NetworkSettings.Ports[relayPortKey], d.Config.Port)
	var hostCfg struct {
		PortBindings map[string][]PortBinding `json:"PortBindings"`
	}
	json.Unmarshal(c.HostConfig, &hostCfg)
	_, pub2 := Bindings(hostCfg.PortBindings[relayPortKey], d.Config.Port)
	for _, p := range pub2 {
		if !slices.Contains(public, p) {
			public = append(public, p)
		}
	}
	d.mu.Lock()
	d.relay, d.relayAt = relay, time.Now()
	d.addrs = addrs
	d.public = len(public) > 0
	d.mu.Unlock()
	switch {
	case len(public) > 0:
		return fmt.Errorf("published on a public address (%s): the direct port is refused until the installer runs again", strings.Join(public, ", "))
	case len(addrs) == 0:
		return fmt.Errorf("Docker published the direct port on none of %v", d.Config.Addrs)
	}
	return nil
}

// Bindings splits Docker's bindings of the sidecar's port into the usable
// addresses (private or tailnet IPv4, on the direct port) and the rest
// (wildcards, public addresses, IPv6, another port), which must not exist.
func Bindings(bs []PortBinding, port int) (usable, public []string) {
	for _, b := range bs {
		a, err := netip.ParseAddr(b.HostIP)
		switch {
		case b.HostIP == "" || err != nil || !direct.Usable(a):
			public = append(public, net.JoinHostPort(firstNonEmptyStr(b.HostIP, "0.0.0.0"), b.HostPort))
		case b.HostPort != strconv.Itoa(port):
			public = append(public, net.JoinHostPort(b.HostIP, b.HostPort))
		case !slices.Contains(usable, a.String()):
			usable = append(usable, a.String())
		}
	}
	return usable, public
}

func firstNonEmptyStr(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func (d *Direct) removeSidecar(ctx context.Context) error {
	c, err := d.Docker.ContainerInspect(ctx, RelayName)
	if IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !isRelay(c) {
		return nil
	}
	return d.Docker.ContainerRemove(ctx, c.ID)
}

// Published is what the self-test probes: the sidecar's usable bindings, and
// whether any isn't; the agent's control address and the sidecar's.
func (d *Direct) Published() (addrs []string, public bool, ctl, relay netip.Addr, state string) {
	if d == nil || d.Config.Port == 0 {
		return nil, false, netip.Addr{}, netip.Addr{}, ""
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.addrs), d.public, d.ctl, d.relay, d.state
}
