package node

import (
	"context"
	"encoding/json"
	"slices"
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

// tokens are the tokens of a sandbox's open sessions, for its report.
func (f *FakeAgent) tokens(sb string) []string {
	var out []string
	for _, t := range f.opened[sb] {
		if t != "" && !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

func (f *FakeAgent) reportOf(sb string, active bool) DirectReport {
	return NewDirectReport(sb, f.sessions(sb), active, f.tokens(sb)...)
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
			f.opened = map[string]map[string]string{}
		}
		var send *DirectReport
		switch verb {
		case "open", "open-quiet":
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
			case has(f.opened[c.Sandbox], c.Session):
				rep["error"] = "used already"
			default:
				if f.opened[c.Sandbox] == nil {
					f.opened[c.Sandbox] = map[string]string{}
				}
				f.opened[c.Sandbox][c.Session] = c.Token
				// open-quiet: as if the open report were lost on the way.
				if verb == "open" {
					r := f.reportOf(c.Sandbox, true)
					send = &r
				}
				rep["ok"] = true
			}
			rep["sandbox"], rep["port"], rep["session"], rep["token"] = c.Sandbox, c.Port, c.Session, c.Token
		case "close":
			for sb, set := range f.opened {
				if has(set, arg) {
					delete(set, arg)
					r := f.reportOf(sb, false)
					send = &r
					rep["ok"], rep["sandbox"] = true, sb
				}
			}
		case "report":
			// report <sandbox> <tk-…,tk-…>: a report naming those tokens, as a
			// misbehaving node could send (the re-review's check of 3).
			sb, list, _ := strings.Cut(arg, " ")
			r := NewDirectReport(sb, f.sessions(sb), false, strings.Split(list, ",")...)
			send = &r
			rep["ok"], rep["tokens"] = true, len(r.Tokens)
		case "active":
			r := f.reportOf(arg, true)
			send = &r
			rep["ok"] = true
		default:
			rep["error"] = "lines are open <ticket>, open-quiet <ticket> (no report), close <session>, active <sandbox> or report <sandbox> <tokens,…>"
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
	for s, t := range f.opened[d.Sandbox] {
		if slices.Contains(d.Sessions, s) || (t != "" && slices.Contains(d.Tokens, t)) {
			delete(f.opened[d.Sandbox], s)
			n++
		}
	}
	var r *DirectReport
	if n > 0 {
		x := f.reportOf(d.Sandbox, false)
		r = &x
	}
	f.dmu.Unlock()
	if r != nil {
		f.send(*r)
	}
	if f.Report != nil {
		f.Report(map[string]any{"directEnd": d.Sessions, "tokens": d.Tokens, "sandbox": d.Sandbox, "ended": n, "at": time.Now().UTC().Format(time.RFC3339)})
	}
	return okResult(op.ID, map[string]int{"ended": n})
}

func has(m map[string]string, k string) bool {
	_, ok := m[k]
	return ok
}
