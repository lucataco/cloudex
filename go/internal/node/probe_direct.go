package node

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/lucataco/cloudex/go/internal/direct"
)

// The fake agent's direct path (v0.2.3 M1), for checking the Worker's side
// live before the real listener exists (M2): it reports a direct fact, keeps
// welcome's secret, checks tickets a client got from GET
// /v1/sandboxes/{ref}/direct with it as the agent will, reports the sessions
// it "opened", and reports each directEnd. Only what it was told to open is
// ever reported.

func (f *FakeAgent) welcome(b []byte) {
	var w Welcome
	if json.Unmarshal(b, &w) != nil {
		return
	}
	rep := map[string]any{"welcome": map[string]any{"direct": w.Direct != nil}}
	if w.Direct != nil {
		secret, err := direct.DecodeSecret(w.Direct.Secret)
		if err != nil {
			rep["error"] = err.Error()
		} else {
			skew := time.Duration(w.Direct.Now-time.Now().UnixMilli()) * time.Millisecond
			f.dmu.Lock()
			f.secret, f.skew = secret, skew
			f.dmu.Unlock()
			rep["welcome"] = map[string]any{"direct": true, "workerClockMs": skew.Milliseconds()}
		}
	}
	if f.Report != nil {
		f.Report(rep)
	}
}

func (f *FakeAgent) sessions(sb string) []string {
	out := []string{}
	for s := range f.opened[sb] {
		out = append(out, s)
	}
	return out
}

func (f *FakeAgent) directLines(ctx context.Context) {
	for {
		var line string
		select {
		case <-ctx.Done():
			return
		case l, ok := <-f.Lines:
			if !ok {
				return
			}
			line = strings.TrimSpace(l)
		}
		verb, arg, _ := strings.Cut(line, " ")
		rep := map[string]any{"line": verb}
		f.dmu.Lock()
		if f.opened == nil {
			f.opened = map[string]map[string]bool{}
		}
		var send *DirectReport
		switch verb {
		case "open":
			c, err := direct.Verify(f.secret, arg)
			switch {
			case f.secret == nil:
				rep["error"] = "no secret from welcome"
			case err != nil:
				rep["error"] = err.Error()
			case c.Node != f.NodeID:
				rep["error"] = "for another node"
			case c.Expired(time.Now().Add(f.skew), 10*time.Second):
				rep["error"] = "expired"
			case f.opened[c.Sandbox][c.Session]:
				rep["error"] = "used already"
			default:
				if f.opened[c.Sandbox] == nil {
					f.opened[c.Sandbox] = map[string]bool{}
				}
				f.opened[c.Sandbox][c.Session] = true
				r := NewDirectReport(c.Sandbox, f.sessions(c.Sandbox), true)
				send = &r
				rep["ok"] = true
			}
			rep["sandbox"], rep["port"], rep["session"] = c.Sandbox, c.Port, c.Session
		case "close":
			for sb, set := range f.opened {
				if set[arg] {
					delete(set, arg)
					r := NewDirectReport(sb, f.sessions(sb), false)
					send = &r
					rep["ok"], rep["sandbox"] = true, sb
				}
			}
		case "active":
			r := NewDirectReport(arg, f.sessions(arg), true)
			send = &r
			rep["ok"] = true
		default:
			rep["error"] = "lines are open <ticket>, close <session> or active <sandbox>"
		}
		f.dmu.Unlock()
		if send != nil {
			if err := f.send(*send); err != nil {
				rep["error"] = "sending the report: " + err.Error()
			} else {
				rep["reported"] = send
			}
		}
		if f.Report != nil {
			f.Report(rep)
		}
	}
}

func (f *FakeAgent) directEnd(op Op) Result {
	b, _ := json.Marshal(op.Args)
	var d DirectEnd
	if json.Unmarshal(b, &d) != nil || !sandboxID.MatchString(d.Sandbox) {
		return errResult(op.ID, "bad arguments")
	}
	f.dmu.Lock()
	n := 0
	for _, s := range d.Sessions {
		if f.opened[d.Sandbox][s] {
			delete(f.opened[d.Sandbox], s)
			n++
		}
	}
	var r *DirectReport
	if n > 0 {
		x := NewDirectReport(d.Sandbox, f.sessions(d.Sandbox), false)
		r = &x
	}
	f.dmu.Unlock()
	if r != nil {
		f.send(*r)
	}
	if f.Report != nil {
		f.Report(map[string]any{"directEnd": d.Sessions, "sandbox": d.Sandbox, "ended": n, "at": time.Now().UTC().Format(time.RFC3339)})
	}
	return okResult(op.ID, map[string]int{"ended": n})
}
