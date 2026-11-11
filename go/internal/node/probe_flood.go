package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// The release checks' misbehaving agents (2026-11-12 review, W1 and W2): a
// node that floods its control socket, and one that floods its streams. They
// run with the node's own key while its agent is stopped (their connection
// replaces the agent's), and say what the deployment did, as JSON, so that
// verify.sh can judge it.

func probeConnect(ctx context.Context, api, nodeID string, key ed25519.PrivateKey) (*websocket.Conn, error) {
	full, signed, err := Endpoint(api, "/v1/node/connect")
	if err != nil {
		return nil, err
	}
	h := http.Header{}
	h.Set(HeaderProtocol, strconv.Itoa(Protocol))
	Signer{Key: key, Node: nodeID}.Sign(h, http.MethodGet, signed, "")
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, res, err := websocket.Dial(dctx, full, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		if res != nil {
			b, _ := io.ReadAll(io.LimitReader(res.Body, 512))
			return nil, fmt.Errorf("connecting: %d %s", res.StatusCode, b)
		}
		return nil, fmt.Errorf("connecting: %w", err)
	}
	c.SetReadLimit(1 << 20)
	return c, nil
}

// probeFacts are a host that passes its self-test and has the image, so
// that the deployment places sandboxes on it.
func probeFacts(cpus int) Facts {
	f := HostFacts(false)
	f.Arch = "amd64"
	f.CPUs = &cpus
	mb := 8192
	f.MemoryMB = &mb
	gb := 100.0
	f.DiskFreeGB = &gb
	f.Isolation = Isolation{Firewall: "pass", CheckedAt: time.Now().UnixMilli()}
	f.Image = &ImageFact{Ref: "ghcr.io/lucataco/cloudex-base:probe", State: "present"}
	return f
}

// FloodReport is what ProbeFlood saw.
type FloodReport struct {
	Facts         int    `json:"facts"`         // facts messages sent
	States        int    `json:"states"`        // state messages sent, for sandboxes that aren't this node's
	TicketsAsked  int    `json:"ticketsAsked"`  // ticket requests sent, for others' sandbox
	TicketsGiven  int    `json:"ticketsGiven"`  // tickets the deployment gave for it
	TicketErrors  string `json:"ticketError"`   // the refusals' reasons, each once
	SecondHello   int    `json:"secondHello"`   // the close code after a second hello (0: still open)
	SecondHelloIn int64  `json:"secondHelloMs"` // how long the close took
}

// ProbeFlood connects as the node, says hello, then sends facts (alternating,
// so each is a change), state messages and ticket requests for other, a
// sandbox that isn't on this node, n of each, as fast as the socket takes
// them; then a second hello, and reports what came back.
func ProbeFlood(ctx context.Context, api, nodeID string, key ed25519.PrivateKey, version, other string, n int) (*FloodReport, error) {
	c, err := probeConnect(ctx, api, nodeID, key)
	if err != nil {
		return nil, err
	}
	defer c.CloseNow()
	wctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	write := func(v any) error {
		b, _ := json.Marshal(v)
		return c.Write(wctx, websocket.MessageText, b)
	}
	rep := &FloodReport{}
	var mu sync.Mutex
	closed := make(chan int, 1)
	go func() {
		for {
			_, b, err := c.Read(wctx)
			if err != nil {
				closed <- int(websocket.CloseStatus(err))
				return
			}
			var t Tickets
			if json.Unmarshal(b, &t) == nil && t.Type == "tickets" {
				mu.Lock()
				rep.TicketsGiven += len(t.Tickets)
				if t.Error != "" && !strings.Contains(rep.TicketErrors, t.Error) {
					rep.TicketErrors = strings.TrimPrefix(rep.TicketErrors+" | "+t.Error, " | ")
				}
				mu.Unlock()
			}
		}
	}()
	if err := write(HelloError(version, probeFacts(2), fmt.Errorf("a probe, which lists no machines"))); err != nil {
		return nil, err
	}
	for i := 0; i < n; i++ {
		if write(FactsMessage{Type: "facts", Facts: probeFacts(2 + i%2)}) == nil {
			rep.Facts++
		}
		if write(StateMessage{Type: "state", Machine: Machine{ID: other, State: []string{"running", "stopped"}[i%2]}}) == nil {
			rep.States++
		}
		if write(TicketRequest{Type: "tickets", ID: fmt.Sprintf("tk-f%d", i), Sandbox: other, Count: 1}) == nil {
			rep.TicketsAsked++
		}
	}
	time.Sleep(3 * time.Second) // the answers
	t0 := time.Now()
	write(HelloError(version, probeFacts(2), fmt.Errorf("a probe, which lists no machines")))
	select {
	case code := <-closed:
		rep.SecondHello = code
		rep.SecondHelloIn = time.Since(t0).Milliseconds()
	case <-time.After(5 * time.Second):
	}
	mu.Lock()
	defer mu.Unlock()
	out := *rep
	return &out, nil
}

// FakeAgent is a node agent with no machines that answers the deployment as
// if it had them: create and start succeed at once, and each exec answers
// exit 0 with no output, so that sandboxes are placed on it, start and run
// their set-up. Execs whose command has a marker misbehave instead:
//   - cx-flood-overrun: stdout far past its credit window (W2);
//   - cx-flood-channel: a frame on channel 3, which the machine never sends on.
//
// A spliced TCP connection (ssh, port 22) with Splice set answers ok and then
// sends without ever reading, as fast as it can, up to SpliceMax bytes.
// Reports go to Report, one JSON line per misbehaving stream: what it sent
// before the deployment closed it, and the close code.
type FakeAgent struct {
	API, NodeID, Version string
	Key                  ed25519.PrivateKey
	Splice               bool
	SpliceMax            int64
	OverrunMax           int64
	Report               func(map[string]any)
	Log                  func(string, ...any)

	send     func(any) error
	mu       sync.Mutex
	waiting  map[string]chan Tickets
	sandbox  map[string]bool
	streamsN atomic.Int64
}

func (f *FakeAgent) logf(format string, args ...any) {
	if f.Log != nil {
		f.Log(format, args...)
	}
}

// Run keeps the fake agent's control socket until ctx ends.
func (f *FakeAgent) Run(ctx context.Context) error {
	c, err := probeConnect(ctx, f.API, f.NodeID, f.Key)
	if err != nil {
		return err
	}
	defer c.CloseNow()
	var wmu sync.Mutex
	f.send = func(v any) error {
		b, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return c.Write(wctx, websocket.MessageText, b)
	}
	f.waiting = map[string]chan Tickets{}
	f.sandbox = map[string]bool{}
	if err := f.send(HelloError(f.Version, probeFacts(4), fmt.Errorf("a probe, which lists no machines"))); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(PingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				wmu.Lock()
				c.Write(ctx, websocket.MessageText, []byte("ping"))
				wmu.Unlock()
			}
		}
	}()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return err
		}
		if string(b) == "pong" {
			continue
		}
		var head struct{ Type string }
		json.Unmarshal(b, &head)
		switch head.Type {
		case "tickets":
			var t Tickets
			json.Unmarshal(b, &t)
			f.mu.Lock()
			ch := f.waiting[t.ID]
			f.mu.Unlock()
			if ch != nil {
				ch <- t
			}
		case "op":
			var op Op
			json.Unmarshal(b, &op)
			go f.answer(ctx, op)
		}
	}
}

func (f *FakeAgent) answer(ctx context.Context, op Op) {
	var sb string
	json.Unmarshal(op.Args["sandbox"], &sb)
	var r Result
	switch op.Name {
	case "create":
		r = okResult(op.ID, MachineInfo{Address: "10.0.0.3", Container: "probe-" + sb, State: "stopped", Created: true})
	case "start":
		f.mu.Lock()
		f.sandbox[sb] = true
		f.mu.Unlock()
		go f.spares(ctx, sb, 4)
		r = okResult(op.ID, MachineInfo{Address: "10.0.0.3", Container: "probe-" + sb, State: "running"})
	case "streams":
		var n int
		json.Unmarshal(op.Args["count"], &n)
		go f.spares(ctx, sb, max(1, min(n, 8)))
		r = okResult(op.ID, struct{}{})
	case "stop", "delete", "ping", "selftest":
		r = okResult(op.ID, struct{}{})
	case "facts":
		r = okResult(op.ID, probeFacts(4))
	default:
		r = errResult(op.ID, "unknown op "+op.Name)
	}
	f.logf("op %s %s", op.Name, sb)
	f.send(r)
}

func (f *FakeAgent) spares(ctx context.Context, sb string, n int) {
	id := "tk-" + randomHex(6)
	ch := make(chan Tickets, 1)
	f.mu.Lock()
	f.waiting[id] = ch
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		delete(f.waiting, id)
		f.mu.Unlock()
	}()
	if f.send(TicketRequest{Type: "tickets", ID: id, Sandbox: sb, Count: n}) != nil {
		return
	}
	var t Tickets
	select {
	case t = <-ch:
	case <-time.After(10 * time.Second):
		return
	}
	for _, raw := range t.Tickets {
		tk, err := readTicket(raw)
		if err != nil {
			continue
		}
		go f.stream(ctx, sb, tk)
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// stream parks one spare and plays its part once it's opened.
func (f *FakeAgent) stream(ctx context.Context, sb string, tk ticket) {
	u, _, _ := Endpoint(f.API, "/v1/node/streams/"+sb+"/"+tk.stream)
	h := http.Header{}
	h.Set(HeaderTicket, tk.raw)
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	c, _, err := websocket.Dial(dctx, u, &websocket.DialOptions{HTTPHeader: h})
	cancel()
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(64 << 20)
	sctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() { // heartbeats, as the agent sends them
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case <-t.C:
				c.Write(sctx, websocket.MessageText, []byte("cx-hb"))
			}
		}
	}()
	_, b, err := c.Read(sctx)
	if err != nil {
		return
	}
	f.streamsN.Add(1)
	go f.spares(ctx, sb, 1) // replace it
	var open struct {
		Exec *struct {
			Argv []string `json:"argv"`
		} `json:"exec"`
		Tcp *struct {
			Port   int  `json:"port"`
			Splice bool `json:"splice"`
		} `json:"tcp"`
		HTTP *json.RawMessage `json:"http"`
	}
	json.Unmarshal(b, &open)
	text := func(s string) error { return c.Write(sctx, websocket.MessageText, []byte(s)) }
	// What the deployment does after the open: the close code, once it closes.
	closed := make(chan int, 1)
	reading := func() {
		for {
			_, _, err := c.Read(sctx)
			if err != nil {
				closed <- int(websocket.CloseStatus(err))
				return
			}
		}
	}
	flood := func(kind string, ch int, limit int64) {
		go reading()
		frame := make([]byte, 256<<10)
		var sent int64
		t0 := time.Now()
		code := -1
	loop:
		for sent < limit {
			select {
			case code = <-closed:
				break loop
			default:
			}
			if ch >= 0 {
				frame[0] = byte(ch)
			}
			wctx, cancel := context.WithTimeout(sctx, 30*time.Second)
			err := c.Write(wctx, websocket.MessageBinary, frame)
			cancel()
			if err != nil {
				select {
				case code = <-closed:
				case <-time.After(5 * time.Second):
				}
				break
			}
			sent += int64(len(frame))
		}
		if code == -1 {
			select {
			case code = <-closed:
			case <-time.After(5 * time.Second):
			}
		}
		if f.Report != nil {
			f.Report(map[string]any{"sandbox": sb, "kind": kind, "sentBytes": sent, "closeCode": code, "ms": time.Since(t0).Milliseconds()})
		}
	}
	switch {
	case open.Exec != nil:
		argv := strings.Join(open.Exec.Argv, " ")
		switch {
		case strings.Contains(argv, "cx-flood-overrun"):
			text(`{"ok":true}`)
			flood("overrun", 1, f.OverrunMax)
		case strings.Contains(argv, "cx-flood-channel"):
			text(`{"ok":true}`)
			flood("channel", 3, 1<<20)
		default:
			text(`{"ok":true}`)
			text(`{"eof":1}`)
			text(`{"eof":2}`)
			text(`{"exit":0}`)
			go reading()
			select {
			case <-closed:
			case <-time.After(30 * time.Second):
			}
		}
	case open.Tcp != nil && open.Tcp.Splice && f.Splice:
		text(`{"ok":true}`)
		flood("splice", -1, f.SpliceMax)
	case open.Tcp != nil:
		text(fmt.Sprintf(`{"refused":"10.0.0.3:%d"}`, open.Tcp.Port))
	default:
		text(`{"error":"the probe answers no HTTP"}`)
	}
}
