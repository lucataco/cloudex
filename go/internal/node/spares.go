package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// Spare stream sockets (M3). For each machine it runs, the agent keeps a few
// stream sockets open to the machine's Sandbox object, parked there until the
// object needs one, so that an exec or a connection doesn't wait for a new
// WebSocket to be set up (272-439 ms in the spikes). It opens a new one as soon
// as one is taken, and more when they're taken quickly.
const (
	SparesMin = 2
	SparesMax = 8
)

var sandboxID = regexp.MustCompile(`^cx-[a-z0-9]{6}$`)

type ticket struct {
	raw     string
	stream  string
	expires time.Time
}

// readTicket reads a ticket's stream id and expiry (it's the Worker's to check).
func readTicket(raw string) (ticket, error) {
	payload, _, ok := strings.Cut(raw, ".")
	if !ok {
		return ticket{}, errors.New("a ticket without a signature")
	}
	b, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return ticket{}, err
	}
	var p struct {
		I string `json:"i"`
		E int64  `json:"e"`
	}
	if err := json.Unmarshal(b, &p); err != nil || p.I == "" {
		return ticket{}, fmt.Errorf("a ticket that can't be read")
	}
	return ticket{raw: raw, stream: p.I, expires: time.Unix(p.E, 0)}, nil
}

// pool keeps one machine's spares.
type pool struct {
	a       *Agent
	sandbox string
	target  Target
	ctx     context.Context
	cancel  context.CancelFunc

	mu      sync.Mutex
	idle    int // dialing or parked
	want    int
	taken   []time.Time
	tickets []ticket
	fails   int
	wake    chan struct{}
}

func (a *Agent) newPool(sandbox string, t Target) *pool {
	ctx, cancel := context.WithCancel(a.runCtx)
	p := &pool{a: a, sandbox: sandbox, target: t, ctx: ctx, cancel: cancel, want: a.spares(), wake: make(chan struct{}, 1)}
	go p.run()
	return p
}

func (p *pool) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// more asks for n more spares now (the object ran out).
func (p *pool) more(n int) {
	p.mu.Lock()
	p.want = min(SparesMax, max(p.want, p.idle+n))
	p.mu.Unlock()
	p.poke()
}

func (p *pool) run() {
	decay := time.NewTicker(30 * time.Second)
	defer decay.Stop()
	for {
		p.mu.Lock()
		for p.idle < p.want {
			p.idle++
			go p.spare()
		}
		p.mu.Unlock()
		select {
		case <-p.ctx.Done():
			return
		case <-p.wake:
		case <-decay.C:
			p.mu.Lock()
			if len(p.taken) == 0 || time.Since(p.taken[len(p.taken)-1]) > 30*time.Second {
				p.want = p.a.spares()
			}
			p.mu.Unlock()
		}
	}
}

// ticket hands out one unexpired ticket, asking the Node object for a few
// more when it has none left.
func (p *pool) ticket() (ticket, error) {
	p.mu.Lock()
	for len(p.tickets) > 0 {
		t := p.tickets[0]
		p.tickets = p.tickets[1:]
		if time.Until(t.expires) > 10*time.Second {
			p.mu.Unlock()
			return t, nil
		}
	}
	p.mu.Unlock()
	raws, err := p.a.requestTickets(p.ctx, p.sandbox, 4)
	if err != nil {
		return ticket{}, err
	}
	var got []ticket
	for _, r := range raws {
		if t, err := readTicket(r); err == nil {
			got = append(got, t)
		}
	}
	if len(got) == 0 {
		return ticket{}, errors.New("the deployment issued no stream tickets")
	}
	p.mu.Lock()
	p.tickets = append(p.tickets, got[1:]...)
	p.mu.Unlock()
	return got[0], nil
}

// spare opens one stream socket, parks it until the object opens it, and
// then serves it.
func (p *pool) spare() {
	s, err := p.dial()
	if err != nil {
		p.mu.Lock()
		p.idle--
		p.fails++
		wait := Backoff(p.fails, p.a.Rand)
		var refused *RefusedError
		if errors.As(err, &refused) && (refused.Status == 403 || refused.Status == 404) {
			wait = max(wait, 30*time.Second)
		}
		p.mu.Unlock()
		if p.ctx.Err() == nil {
			p.a.logf("%s: opening a spare stream: %v; again in %v", p.sandbox, err, wait.Round(time.Millisecond))
			select {
			case <-p.ctx.Done():
			case <-time.After(wait):
				p.poke()
			}
		}
		return
	}
	p.mu.Lock()
	p.fails = 0
	p.mu.Unlock()
	go s.heartbeat()
	in, err := s.Read()
	p.mu.Lock()
	p.idle--
	if err == nil {
		// Taken: replace it now, and keep more while they go quickly.
		now := time.Now()
		p.taken = append(p.taken, now)
		for len(p.taken) > 0 && now.Sub(p.taken[0]) > 2*time.Second {
			p.taken = p.taken[1:]
		}
		if len(p.taken) >= 2 {
			p.want = min(SparesMax, p.want+2)
		}
	}
	p.mu.Unlock()
	p.poke()
	if err != nil {
		s.Abort()
		return
	}
	if in.Ctl == nil || (in.Ctl.Exec == nil && in.Ctl.TCP == nil && in.Ctl.HTTP == nil) {
		s.Close(StreamBadMessage, "the first message opens an exec, tcp or http")
		return
	}
	defer s.Abort()
	t0 := time.Now()
	p.a.handler().Serve(s, p.target, *in.Ctl)
	if p.a.Debug {
		p.a.logf("stream %s: %s served in %v", s.ID, openKind(*in.Ctl), time.Since(t0).Round(time.Millisecond))
	}
}

func (p *pool) dial() (*Stream, error) {
	t, err := p.ticket()
	if err != nil {
		return nil, err
	}
	full, _, err := Endpoint(p.a.API, "/v1/node/streams/"+p.sandbox+"/"+t.stream)
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set(HeaderTicket, t.raw)
	dctx, cancel := context.WithTimeout(p.ctx, 15*time.Second)
	defer cancel()
	c, res, err := websocket.Dial(dctx, full, &websocket.DialOptions{HTTPHeader: h, HTTPClient: p.a.HTTP})
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
	c.SetReadLimit(frameMax + 1024)
	return newStream(p.ctx, t.stream, c), nil
}

// HeaderTicket carries a stream's ticket.
const HeaderTicket = "x-cx-ticket"

// ---------------------------------------------------------------- pools

// startPool keeps spares for a sandbox's machine while it runs.
func (a *Agent) startPool(sandbox string) {
	if a.Runtime == nil || a.runCtx == nil {
		return
	}
	ctx, cancel := context.WithTimeout(a.runCtx, 10*time.Second)
	t, running, err := a.Runtime.Target(ctx, sandbox)
	cancel()
	if err != nil || !running || t.Address == "" {
		return
	}
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	if old := a.pools[sandbox]; old != nil {
		if old.target == t {
			return
		}
		old.cancel()
	}
	a.pools[sandbox] = a.newPool(sandbox, t)
}

func (a *Agent) stopPool(sandbox string) {
	a.poolMu.Lock()
	defer a.poolMu.Unlock()
	if p := a.pools[sandbox]; p != nil {
		p.cancel()
		delete(a.pools, sandbox)
	}
}

// startPools brings back the spares of the machines that run (after the
// agent restarted, they kept running).
func (a *Agent) startPools() {
	ctx, cancel := context.WithTimeout(a.runCtx, 30*time.Second)
	ms, err := a.Runtime.Machines(ctx)
	cancel()
	if err != nil {
		a.logf("listing machines: %v", err)
		return
	}
	for _, m := range ms {
		if m.State == "running" {
			a.startPool(m.ID)
		}
	}
}

func openKind(m StreamMsg) string {
	switch {
	case m.Exec != nil:
		return "exec " + strings.Join(m.Exec.Argv[:min(len(m.Exec.Argv), 3)], " ")
	case m.TCP != nil:
		return fmt.Sprintf("tcp %d", m.TCP.Port)
	case m.HTTP != nil:
		return fmt.Sprintf("http %d %s", m.HTTP.Port, m.HTTP.Method)
	}
	return "?"
}
