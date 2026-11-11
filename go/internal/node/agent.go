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
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return &RefusedError{Status: status, Message: e.Error}
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

	status   Status
	statusMu sync.Mutex
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
	return HostFacts(a.Unisolated)
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
			attempt++
			a.logf("connect attempt %d failed after %v: %v; trying again in %v", attempt, time.Since(t0).Round(time.Millisecond), err, d.Round(time.Millisecond))
			a.setStatus(func(s *Status) {
				if s.Connected || s.Since == 0 {
					s.Since = time.Now().UnixMilli()
				}
				s.Connected, s.Attempts, s.LastError = false, attempt, err.Error()
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
			d = RefusedWait
			why = "this node was removed from the deployment (cloudex node rm); its key no longer works. Add it again with cloudex node add, then join with the new token"
		case CloseReplaced:
			d = ReplacedWait
			why = "another connection with this node's key took over: is a second agent running with the same state directory?"
		}
		a.logf("disconnected: %s; connecting again in %v", why, d.Round(time.Millisecond))
		a.setStatus(func(s *Status) { s.Connected, s.Since, s.LastError, s.Attempts = false, down.UnixMilli(), why, 0 })
		if err := a.wait(ctx, d); err != nil {
			return err
		}
	}
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
	if err := send(NewHello(a.Version, a.facts(), nil)); err != nil {
		return "sending hello: " + err.Error(), 0
	}
	now := time.Now()
	var lastPong atomic.Int64
	lastPong.Store(now.UnixMilli())
	a.setStatus(func(s *Status) {
		s.Connected, s.Since, s.LastPongAt, s.LastError, s.Attempts = true, now.UnixMilli(), now.UnixMilli(), "", 0
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
		var op Op
		if json.Unmarshal(data, &op) != nil || op.Type != "op" || op.ID == "" {
			a.logf("ignoring a message it doesn't know: %.200s", data)
			continue
		}
		go func() {
			if err := send(a.answer(op)); err != nil {
				a.logf("answering %s %s: %v", op.Name, op.ID, err)
			}
		}()
	}
}

// answer runs an op. The set is fixed; nothing here runs anything on the host.
func (a *Agent) answer(op Op) Result {
	switch op.Name {
	case "ping":
		return okResult(op.ID, struct{}{})
	case "facts":
		return okResult(op.ID, a.facts())
	default:
		return errResult(op.ID, "unknown op "+op.Name+": this agent may be older than the deployment")
	}
}
