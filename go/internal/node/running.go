package node

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

// RunningExec is an exec the agent started and hasn't seen end. The list is
// kept in the state directory (execs.json), so that an agent that died
// (killed, or its host's Docker restarted it) hangs up what its last run left:
// Docker never ends an exec whose client went away (M0 Q5).
type RunningExec struct {
	Tag       string `json:"tag"`
	Container string `json:"container"`
	ExecID    string `json:"execId"`
	Pty       bool   `json:"pty"`
	Since     int64  `json:"since"`
}

type execRegistry struct {
	state State
	mu    sync.Mutex
	execs map[string]RunningExec
}

func newExecRegistry(s State) *execRegistry {
	return &execRegistry{state: s, execs: map[string]RunningExec{}}
}

func (r *execRegistry) save() {
	if r.state.Dir == "" {
		return
	}
	list := make([]RunningExec, 0, len(r.execs))
	for _, e := range r.execs {
		list = append(list, e)
	}
	r.state.write("execs.json", list, 0o600)
}

func (r *execRegistry) add(e RunningExec) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.execs[e.Tag] = e
	r.save()
}

func (r *execRegistry) remove(tag string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.execs, tag)
	r.save()
}

// leftovers reads what an earlier run of the agent left in execs.json.
func (s State) leftovers() []RunningExec {
	var list []RunningExec
	if b, err := readFile(s.path("execs.json")); err == nil {
		json.Unmarshal(b, &list)
	}
	return list
}

// hangUpLeftovers ends the execs an earlier run left running, as a dropped
// stream would have: SIGHUP for a terminal, SIGTERM otherwise, then SIGKILL.
func (h *Handler) hangUpLeftovers(s State) {
	list := s.leftovers()
	if len(list) == 0 {
		return
	}
	s.write("execs.json", []RunningExec{}, 0o600)
	for _, e := range list {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		st, err := h.Docker.ExecInspect(ctx, e.ExecID)
		cancel()
		if err != nil || !st.Running {
			continue
		}
		h.logf("exec %s was left running by an earlier run of the agent; hanging it up", short(e.ExecID))
		var pty *Pty
		if e.Pty {
			pty = &Pty{}
		}
		h.hangUp(Target{Container: e.Container}, e.ExecID, e.Tag, StreamExec{Pty: pty})
	}
}
