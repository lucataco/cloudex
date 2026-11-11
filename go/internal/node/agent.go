package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// The agent pings every PingEvery, and the Node object answers "pong"
// without waking. With no pong for PongDead, the connection is taken for dead
// and replaced: after an edge reset neither end may hear about it (spikes,
// "Half-open sockets").
const (
	PingEvery = 10 * time.Second
	PongDead  = 35 * time.Second
)

// Endpoint builds the URL of an API route, and the path with query that is signed.
func Endpoint(api, route string) (full, signed string, err error) {
	u, err := url.Parse(api)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", "", fmt.Errorf("bad API URL %q: give it as https://<your deployment>", api)
	}
	u = u.JoinPath(route)
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	signed = u.EscapedPath()
	if u.RawQuery != "" {
		signed += "?" + u.RawQuery
	}
	return u.String(), signed, nil
}

// apiError reads the API's {"error": "..."} answer.
func apiError(status int, body []byte) error {
	var e struct {
		Error   string `json:"error"`
		Revoked bool   `json:"revoked"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return &RefusedError{Status: status, Message: e.Error, Revoked: status == http.StatusGone && e.Revoked}
	}
	msg := string(bytes.TrimSpace(body))
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return &RefusedError{Status: status, Message: fmt.Sprintf("HTTP %d %s", status, msg)}
}

// RefusedError is an answer from the API that isn't a success.
type RefusedError struct {
	Status  int
	Message string
	// Revoked: the deployment said, to a request signed with this node's own
	// key, that the node was removed (410 with "revoked": true).
	Revoked bool
}

func (e *RefusedError) Error() string { return e.Message }

// JoinNode trades a join token for the node's place: POST /v1/node/join,
// signed with key, which the deployment keeps the public half of.
func JoinNode(ctx context.Context, hc *http.Client, api, token, version string, key ed25519.PrivateKey, facts Facts) (*Joined, error) {
	full, signed, err := Endpoint(api, "/v1/node/join")
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(Join{Token: token, PublicKey: PublicKey(key), Version: version, Facts: facts})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, full, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	Signer{Key: key}.Sign(req.Header, http.MethodPost, signed, "")
	if hc == nil {
		hc = http.DefaultClient
	}
	res, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("can't reach the API at %s: %w", api, err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != http.StatusOK {
		return nil, apiError(res.StatusCode, data)
	}
	var j Joined
	if err := json.Unmarshal(data, &j); err != nil || j.Node.ID == "" {
		return nil, fmt.Errorf("the API answered the join with something else: %.200s", data)
	}
	return &j, nil
}

// Agent keeps the node's control socket open.
type Agent struct {
	API        string
	ID         string
	Key        ed25519.PrivateKey
	Version    string
	Unisolated bool
	State      State
	Log        *log.Logger

	// Replaced in tests.
	PingEvery time.Duration
	PongDead  time.Duration
	Rand      *rand.Rand
	Facts     func() Facts
	HTTP      *http.Client
	// Wait, when set, replaces sleeping between attempts.
	Wait func(ctx context.Context, d time.Duration) error

	// The Docker runtime (M4): machines, their networks, and the isolation
	// self-test. Nil when the agent runs without a machine pool.
	Runtime *Runtime
	Docker  *Docker
	Spares  int  // spare stream sockets kept for each running machine (SparesMin by default)
	Debug   bool // logs each stream
	// SelfTestEvery replaces the hour between self-tests (tests).
	SelfTestEvery time.Duration

	status   Status
	statusMu sync.Mutex

	runCtx context.Context
	poolMu sync.Mutex
	pools  map[string]*pool
	hand   *Handler

	ctlMu   sync.Mutex
	ctlSend func(v any) error // the current control connection's, or nil
	waiting map[string]chan Tickets

	testNow chan struct{} // runs the self-test now
	testMu  sync.Mutex

	// cloudex.internal and the provider hosts for machines (M6, egress.go),
	// and the node CA their TLS ends with (ca.go).
	egress    *Egress
	egressNow chan struct{}
	nodeCA    atomic.Pointer[CA]
}

// CA is the node CA now; nil until the deployment has named its provider hosts.
func (a *Agent) CA() *CA { return a.nodeCA.Load() }

// setProviders makes sure the node CA covers exactly the deployment's
// provider hosts (its welcome message), making a new one when they changed.
func (a *Agent) setProviders(hosts []string) {
	if len(hosts) == 0 {
		return
	}
	ca, made, err := a.State.EnsureCA(a.ID, hosts)
	if err != nil {
		a.logf("the node CA: %v", err)
		return
	}
	if made {
		a.logf("made the node CA for %d provider hosts (sha256 %s); machines trust it from their next start", len(ca.Hosts), ca.Fingerprint()[:16])
	}
	a.nodeCA.Store(ca)
}

func (a *Agent) docker() *Docker {
	if a.Docker == nil {
		a.Docker = &Docker{}
	}
	return a.Docker
}

func (a *Agent) handler() *Handler {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	if a.hand == nil {
		a.hand = &Handler{Docker: a.docker(), Log: a.Log, Debug: a.Debug, running: newExecRegistry(a.State)}
	}
	return a.hand
}

func (a *Agent) spares() int {
	if a.Spares > 0 {
		return min(a.Spares, SparesMax)
	}
	return SparesMin
}

// requestTickets asks the Node object, over the control socket, for stream
// tickets for a sandbox.
func (a *Agent) requestTickets(ctx context.Context, sandbox string, n int) ([]string, error) {
	a.ctlMu.Lock()
	send := a.ctlSend
	id := fmt.Sprintf("tk-%x", rand.Uint64())
	ch := make(chan Tickets, 1)
	if send != nil {
		if a.waiting == nil {
			a.waiting = map[string]chan Tickets{}
		}
		a.waiting[id] = ch
	}
	a.ctlMu.Unlock()
	if send == nil {
		return nil, errors.New("not connected to the deployment")
	}
	defer func() {
		a.ctlMu.Lock()
		delete(a.waiting, id)
		a.ctlMu.Unlock()
	}()
	if err := send(TicketRequest{Type: "tickets", ID: id, Sandbox: sandbox, Count: min(n, StreamTicketsMax)}); err != nil {
		return nil, err
	}
	t := time.NewTimer(10 * time.Second)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.C:
		return nil, errors.New("the deployment didn't answer a ticket request within 10 s")
	case r := <-ch:
		if r.Error != "" {
			return nil, errors.New(r.Error)
		}
		return r.Tickets, nil
	}
}

func (a *Agent) logf(format string, args ...any) {
	if a.Log != nil {
		a.Log.Printf(format, args...)
	}
}

func (a *Agent) facts() Facts {
	if a.Facts != nil {
		return a.Facts()
	}
	f := HostFacts(a.Unisolated)
	if a.Runtime != nil {
		f = a.Runtime.Facts(f)
	}
	return f
}

// send sends a message on the control socket, if it's up.
func (a *Agent) send(v any) {
	a.ctlMu.Lock()
	send := a.ctlSend
	a.ctlMu.Unlock()
	if send != nil {
		if err := send(v); err != nil {
			a.logf("sending %T: %v", v, err)
		}
	}
}

func (a *Agent) setStatus(change func(*Status)) {
	a.statusMu.Lock()
	defer a.statusMu.Unlock()
	change(&a.status)
	a.status.PID, a.status.APIURL, a.status.NodeID, a.status.Version = os.Getpid(), a.API, a.ID, a.Version
	if a.State.Dir != "" {
		if err := a.State.SaveStatus(a.status); err != nil {
			a.logf("can't write status: %v", err)
		}
	}
}

func (a *Agent) wait(ctx context.Context, d time.Duration) error {
	if a.Wait != nil {
		return a.Wait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Run keeps the control socket up until ctx ends.
func (a *Agent) Run(ctx context.Context) error {
	a.runCtx = ctx
	a.poolMu.Lock()
	a.pools = map[string]*pool{}
	a.poolMu.Unlock()
	a.testNow = make(chan struct{}, 1)
	if r := a.Runtime; r != nil {
		r.NodeID = a.ID
		r.Unisolated = a.Unisolated
		r.OnFacts = func() { a.send(FactsMessage{Type: "facts", Facts: a.facts()}) }
		r.OnState = func(m Machine) {
			a.kickEgress()
			if m.State == "running" {
				a.startPool(m.ID)
			} else {
				a.stopPool(m.ID)
			}
			a.send(StateMessage{Type: "state", Machine: m})
		}
		if img := a.State.Image(); img != "" && r.ImageOverride == "" {
			r.SetImage(img)
		} else if r.ImageOverride != "" {
			r.SetImage(r.ImageOverride)
		}
		go a.handler().hangUpLeftovers(a.State)
		go func() {
			// What a self-test left when the agent died goes first, then the
			// first self-test of this run starts.
			sctx, cancel := context.WithTimeout(ctx, time.Minute)
			r.Sweep(sctx)
			cancel()
			a.startPools()
			a.selfTests(ctx)
		}()
		go r.Watch(ctx)
		r.LastSubnet = a.State.LastSubnet
		r.SaveSubnet = func(p netip.Prefix) {
			if err := a.State.SaveLastSubnet(p); err != nil {
				a.logf("keeping the last subnet: %v", err)
			}
		}
		go r.Guard(ctx, a.kickSelfTest)
		if ca, err := a.State.LoadCA(); err == nil && ca != nil {
			a.nodeCA.Store(ca)
		} else if err != nil {
			a.logf("the node CA: %v; a new one is made when the deployment names its providers", err)
		}
		hc := a.HTTP
		if hc == nil {
			hc = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 15 * time.Second}}
		}
		a.egress = &Egress{API: a.API, Node: a.ID, Key: a.Key, HTTP: hc, Log: a.Log, Debug: a.Debug, CA: a.CA, Version: a.Version}
		a.egressNow = make(chan struct{}, 1)
		go a.egressLoop(ctx)
	}
	var down time.Time
	attempt := 0
	for {
		t0 := time.Now()
		c, err := a.dial(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			d := Backoff(attempt, a.Rand)
			var refused *RefusedError
			if errors.As(err, &refused) && (refused.Status == 401 || refused.Status == 403 || refused.Status == 426) {
				d = max(d, RefusedWait)
			}
			if errors.As(err, &refused) && refused.Status == 429 {
				d = max(d, TooOftenWait) // the Node object limits connects a minute
			}
			if errors.As(err, &refused) && refused.Revoked {
				d = RevokedWait
				a.revoked(ctx, refused.Message)
			}
			attempt++
			a.logf("connect attempt %d failed after %v: %v; trying again in %v", attempt, time.Since(t0).Round(time.Millisecond), err, d.Round(time.Millisecond))
			a.setStatus(func(s *Status) {
				if s.Connected || s.Since == 0 {
					s.Since = time.Now().UnixMilli()
				}
				s.Connected, s.Attempts, s.LastError = false, attempt, err.Error()
				s.Revoked = refused != nil && refused.Revoked
			})
			if err := a.wait(ctx, d); err != nil {
				return err
			}
			continue
		}
		if down.IsZero() {
			a.logf("connected in %v", time.Since(t0).Round(time.Millisecond))
		} else {
			a.logf("connected again after %v down (%d attempts)", time.Since(down).Round(time.Millisecond), attempt+1)
		}
		attempt = 0
		why, code := a.session(ctx, c)
		down = time.Now()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d := Backoff(0, a.Rand)
		switch code {
		case CloseRevoked:
			d = RevokedWait
			why = "this node was removed from the deployment (cloudex node rm); its key no longer works. Add it again with cloudex node add, then join with the new token"
			a.revoked(ctx, why)
		case CloseReplaced:
			d = ReplacedWait
			why = "another connection with this node's key took over: is a second agent running with the same state directory?"
		}
		a.logf("disconnected: %s; connecting again in %v", why, d.Round(time.Millisecond))
		a.setStatus(func(s *Status) {
			s.Connected, s.Since, s.LastError, s.Attempts = false, down.UnixMilli(), why, 0
			s.Revoked = code == CloseRevoked
		})
		if err := a.wait(ctx, d); err != nil {
			return err
		}
	}
}

// revoked acts on the deployment's word that this node was removed (review
// R3): a 410 with "revoked" to a connect signed with the node's own key, or
// close code 4003 on its control socket. Its machines stop, since nothing
// may run them any more; their disks stay for the installer's --uninstall.
func (a *Agent) revoked(ctx context.Context, why string) {
	n := 0
	if a.Runtime != nil {
		sctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		n = a.Runtime.StopAll(sctx, "its node was removed from the deployment (cloudex node rm)")
		cancel()
	}
	a.logf("REVOKED: %s. Stopped %d running machines; their disks stay on this host until you uninstall the agent (node-install.sh --uninstall)", why, n)
	a.setStatus(func(s *Status) { s.Revoked = true; s.Connected = false; s.LastError = why })
}

func (a *Agent) dial(ctx context.Context) (*websocket.Conn, error) {
	full, signed, err := Endpoint(a.API, "/v1/node/connect")
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set(HeaderProtocol, strconv.Itoa(Protocol))
	Signer{Key: a.Key, Node: a.ID}.Sign(h, http.MethodGet, signed, "")
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, res, err := websocket.Dial(dctx, full, &websocket.DialOptions{HTTPHeader: h, HTTPClient: a.HTTP})
	if err != nil {
		if res != nil && res.StatusCode != http.StatusSwitchingProtocols {
			var body []byte
			if res.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(res.Body, 1024))
			}
			return nil, apiError(res.StatusCode, body)
		}
		return nil, err
	}
	c.SetReadLimit(1 << 20)
	return c, nil
}

// session runs one connection until it ends, and says why.
func (a *Agent) session(ctx context.Context, c *websocket.Conn) (string, int) {
	defer c.CloseNow()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	send := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		wctx, wcancel := context.WithTimeout(sctx, 10*time.Second)
		defer wcancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	if err := send(a.hello()); err != nil {
		return "sending hello: " + err.Error(), 0
	}
	a.ctlMu.Lock()
	a.ctlSend = send
	a.ctlMu.Unlock()
	defer func() {
		a.ctlMu.Lock()
		a.ctlSend = nil
		a.ctlMu.Unlock()
	}()
	now := time.Now()
	var lastPong atomic.Int64
	lastPong.Store(now.UnixMilli())
	a.setStatus(func(s *Status) {
		s.Connected, s.Since, s.LastPongAt, s.LastError, s.Attempts, s.Revoked = true, now.UnixMilli(), now.UnixMilli(), "", 0, false
	})

	every, dead := a.PingEvery, a.PongDead
	if every == 0 {
		every = PingEvery
	}
	if dead == 0 {
		dead = PongDead
	}
	stale := make(chan struct{})
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				if time.Since(time.UnixMilli(lastPong.Load())) > dead {
					close(stale)
					cancel()
					return
				}
				wctx, wcancel := context.WithTimeout(sctx, 10*time.Second)
				c.Write(wctx, websocket.MessageText, []byte("ping"))
				wcancel()
			}
		}
	}()

	for {
		typ, data, err := c.Read(sctx)
		if err != nil {
			select {
			case <-stale:
				c.Close(websocket.StatusCode(CloseStale), "no pong")
				return fmt.Sprintf("no pong for %v", dead), CloseStale
			default:
			}
			code := int(websocket.CloseStatus(err))
			if code > 0 {
				var ce websocket.CloseError
				if errors.As(err, &ce) && ce.Reason != "" {
					return fmt.Sprintf("closed by the deployment (%d %s)", code, ce.Reason), code
				}
				return fmt.Sprintf("closed by the deployment (%d)", code), code
			}
			return err.Error(), 0
		}
		if typ != websocket.MessageText {
			continue
		}
		if string(data) == "pong" {
			t := time.Now().UnixMilli()
			lastPong.Store(t)
			a.setStatus(func(s *Status) { s.LastPongAt = t })
			continue
		}
		var head struct{ Type string }
		json.Unmarshal(data, &head)
		if head.Type == "welcome" {
			var w Welcome
			if json.Unmarshal(data, &w) == nil && w.Image != "" && a.Runtime != nil {
				if a.State.Image() != w.Image {
					a.State.SaveImage(w.Image)
				}
				a.Runtime.SetImage(w.Image)
				a.kickSelfTest()
			}
			if a.Runtime != nil && len(w.Providers) > 0 {
				a.setProviders(w.Providers)
			}
			continue
		}
		if head.Type == "tickets" {
			var t Tickets
			if json.Unmarshal(data, &t) == nil {
				a.ctlMu.Lock()
				ch := a.waiting[t.ID]
				a.ctlMu.Unlock()
				if ch != nil {
					ch <- t
				}
			}
			continue
		}
		var op Op
		if json.Unmarshal(data, &op) != nil || op.Type != "op" || op.ID == "" {
			a.logf("ignoring a message it doesn't know: %.200s", data)
			continue
		}
		// Both in their own goroutine: a go statement evaluates its arguments
		// in this one, and an op can take minutes (a start waits up to 90 s
		// for a machine that may be hostile), while pongs must still be read.
		go func() { a.reply(op, a.answer(op)) }()
	}
}

// AnswerFor is how long the agent keeps trying to deliver an op's answer
// after the connection it came on dropped (v0.2.2 M5).
var AnswerFor = 30 * time.Second

// reply sends an op's answer on the control connection that's up now, which
// may be a newer one than the op came on: a reconnect while a start or create
// runs (an edge reset, a deploy) mustn't lose the answer, which the Node
// object still waits for. Op ids are unique, so an answer the Node object
// isn't waiting for any more is dropped there.
func (a *Agent) reply(op Op, r Result) {
	deadline := time.Now().Add(AnswerFor)
	for {
		a.ctlMu.Lock()
		send := a.ctlSend
		a.ctlMu.Unlock()
		err := errors.New("not connected")
		if send != nil {
			if err = send(r); err == nil {
				return
			}
		}
		if time.Now().After(deadline) || a.runCtx == nil || a.runCtx.Err() != nil {
			a.logf("answering %s %s: %v", op.Name, op.ID, err)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// hello says who the agent is and lists the machines it runs, which the
// Worker reconciles with their sandboxes (v0.2.2 M5). A list it couldn't make
// says so, so that no sandbox is taken for gone.
func (a *Agent) hello() Hello {
	if a.Runtime == nil {
		// Without a runtime (a probe, or an agent run outside its container)
		// it can't see the machines: an empty list would take them for gone.
		return HelloError(a.Version, a.facts(), errors.New("this agent runs without a machine runtime, so it can't list the machines"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ms, err := a.Runtime.Machines(ctx)
	if err != nil {
		a.logf("listing machines for hello: %v", err)
		return HelloError(a.Version, a.facts(), err)
	}
	return NewHello(a.Version, a.facts(), ms)
}

func (a *Agent) kickSelfTest() {
	select {
	case a.testNow <- struct{}{}:
	default:
	}
}

// selfTests runs the isolation self-test at start, every hour, and when
// asked (a new image, the selftest op), and keeps its result as the node's
// isolation.
func (a *Agent) selfTests(ctx context.Context) {
	every := a.SelfTestEvery
	if every == 0 {
		every = SelfTestEvery
	}
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.testNow:
		}
		if a.Runtime.Image() == "" {
			t.Reset(every) // the welcome message kicks it
			continue
		}
		rep := a.runSelfTest(ctx)
		if rep.Error != "" {
			// It couldn't run (Docker was busy, the image is still coming):
			// that's no verdict on the firewall, so again soon.
			t.Reset(min(every, time.Minute))
			continue
		}
		t.Reset(every)
	}
}

func (a *Agent) runSelfTest(ctx context.Context) *SelfTestReport {
	a.testMu.Lock()
	defer a.testMu.Unlock()
	tctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	rep := a.Runtime.SelfTest(tctx)
	if ctx.Err() != nil {
		return rep
	}
	a.Runtime.Record(rep)
	if rep.Pass {
		a.logf("isolation self-test passed: %d checks in %v", len(rep.Checks), time.Duration(rep.TookMs)*time.Millisecond)
	} else {
		a.logf("isolation self-test FAILED; this node refuses new machines: %s", strings.Join(rep.Failed(), "; "))
	}
	if rep.Error == "" && rep.Breach() && !a.Unisolated {
		// Something a machine mustn't reach answered: the running ones stop
		// too, not only new ones (review H3).
		sctx, scancel := context.WithTimeout(ctx, time.Minute)
		n := a.Runtime.StopAll(sctx, "this node's isolation self-test found the firewall open ("+truncate(strings.Join(rep.Failed(), "; "), 200)+"), so its running machines were stopped. Resume the sandbox once the node's self-test passes again (cloudex node info).")
		scancel()
		a.logf("stopped %d running machines while the isolation self-test fails", n)
	}
	iso := a.facts().Isolation
	a.setStatus(func(s *Status) { s.Isolation = &iso })
	return rep
}

// answer runs an op. The set is fixed; nothing here runs anything on the host.
func (a *Agent) answer(op Op) Result {
	var args struct {
		Sandbox string `json:"sandbox"`
		Count   int    `json:"count"`
		Hold    bool   `json:"hold"`
	}
	b, _ := json.Marshal(op.Args)
	json.Unmarshal(b, &args)
	switch op.Name {
	case "ping":
		return okResult(op.ID, struct{}{})
	case "facts":
		return okResult(op.ID, a.facts())
	case "streams":
		if !sandboxID.MatchString(args.Sandbox) {
			return errResult(op.ID, "not a sandbox id")
		}
		a.poolMu.Lock()
		p := a.pools[args.Sandbox]
		a.poolMu.Unlock()
		if p == nil {
			return errResult(op.ID, "no machine here runs "+args.Sandbox)
		}
		p.more(max(1, min(args.Count, SparesMax)))
		return okResult(op.ID, struct{}{})
	}
	switch op.Name {
	case "create", "start", "stop", "delete", "selftest":
	default:
		return errResult(op.ID, "unknown op "+op.Name+": this agent may be older than the deployment")
	}
	r := a.Runtime
	if r == nil {
		return errResult(op.ID, errNoRuntime.Error())
	}
	ctx, cancel := context.WithTimeout(a.runCtx, 3*time.Minute)
	defer cancel()
	switch op.Name {
	case "create":
		var req MachineRequest
		if err := json.Unmarshal(b, &req); err != nil {
			return errResult(op.ID, "bad arguments: "+err.Error())
		}
		m, err := r.Create(ctx, req)
		a.kickEgress()
		if err != nil {
			return errResult(op.ID, err.Error())
		}
		return okResult(op.ID, m)
	case "start":
		m, err := r.Start(ctx, args.Sandbox, args.Hold)
		a.kickEgress()
		if err != nil {
			return errResult(op.ID, err.Error())
		}
		if ca := a.CA(); ca != nil {
			m.CA = string(ca.PEM)
		}
		a.startPool(args.Sandbox)
		return okResult(op.ID, m)
	case "stop":
		a.stopPool(args.Sandbox)
		if err := r.Stop(ctx, args.Sandbox); err != nil {
			return errResult(op.ID, err.Error())
		}
		return okResult(op.ID, struct{}{})
	case "delete":
		a.stopPool(args.Sandbox)
		// Its listeners and their connections go before its network does
		// (review H8), not at the next sync.
		if a.egress != nil {
			a.egress.Drop(args.Sandbox)
		}
		err := r.Delete(ctx, args.Sandbox)
		a.kickEgress()
		if err != nil {
			return errResult(op.ID, err.Error())
		}
		return okResult(op.ID, struct{}{})
	case "selftest":
		rep := a.runSelfTest(ctx)
		return okResult(op.ID, map[string]any{"pass": rep.Pass, "failed": rep.Failed(), "checks": len(rep.Checks), "tookMs": rep.TookMs})
	}
	return errResult(op.ID, "unknown op "+op.Name+": this agent may be older than the deployment")
}
