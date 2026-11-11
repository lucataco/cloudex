package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// A process gets this long after its hang-up signal before it's killed.
const killGrace = 10 * time.Second

// After a process exits, its exit waits for output still on its way: until
// the output ends, or has been quiet for outputIdle, for at most exitWaitMax.
const (
	outputIdle  = 200 * time.Millisecond
	exitWaitMax = 10 * time.Second
)

// Target is the machine a stream reaches: a container on this host.
type Target struct {
	Container string // its id or name
	Address   string // its IPv4 address on the host
}

// Handler runs what a stream opens, against a machine.
type Handler struct {
	Docker  *Docker
	Log     *log.Logger
	Debug   bool
	running *execRegistry
}

func (h *Handler) debugf(format string, args ...any) {
	if h.Debug {
		h.logf(format, args...)
	}
}

func (h *Handler) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log.Printf(format, args...)
	}
}

// Serve runs one opened stream to its end and closes it.
func (h *Handler) Serve(s *Stream, m Target, open StreamMsg) {
	switch {
	case open.Exec != nil:
		h.exec(s, m, *open.Exec)
	case open.TCP != nil:
		h.tcp(s, m, *open.TCP)
	case open.HTTP != nil:
		h.http(s, m, *open.HTTP)
	default:
		s.Close(StreamBadMessage, "the first message opens an exec, tcp or http")
	}
}

func (h *Handler) fail(s *Stream, why string) {
	if len(why) > 2000 {
		why = why[:2000]
	}
	s.Send(StreamMsg{Error: &why})
	s.Close(1000, "failed")
}

func newExecTag() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// stdinPump writes what arrives on channel 0 to w in order, granting the
// object credit for each piece once it's written, and closes with eof.
type stdinPump struct {
	ch   chan []byte
	once sync.Once
}

func newStdinPump(s *Stream, w io.Writer, closeWrite func() error) *stdinPump {
	p := &stdinPump{ch: make(chan []byte, 256)}
	go func() {
		broken := false
		for b := range p.ch {
			if b == nil {
				closeWrite()
				continue
			}
			if !broken {
				if _, err := w.Write(b); err != nil {
					broken = true // the process stopped reading; keep granting so nothing hangs
				}
			}
			s.Consumed(ChStdin, len(b))
		}
	}()
	return p
}

func (p *stdinPump) push(b []byte) { p.ch <- b }
func (p *stdinPump) eof()          { p.ch <- nil }
func (p *stdinPump) stop()         { p.once.Do(func() { close(p.ch) }) }

// exec runs a command with the Engine API, tagged with CX_EXEC so that its
// session can be found and signalled (Docker can't signal an exec; M0 Q5).
func (h *Handler) exec(s *Stream, m Target, e StreamExec) {
	ctx := s.ctx
	tag := newExecTag()
	env := make([]string, 0, len(e.Env)+1)
	keys := make([]string, 0, len(e.Env))
	for k := range e.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "CX_EXEC" {
			continue
		}
		env = append(env, k+"="+e.Env[k])
	}
	env = append(env, "CX_EXEC="+tag)
	var size *[2]int
	if e.Pty != nil {
		size = &[2]int{e.Pty.Rows, e.Pty.Cols}
	}
	t0 := time.Now()
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	id, err := h.Docker.ExecCreate(cctx, m.Container, ExecConfig{
		AttachStdin: e.Stdin, AttachStdout: true, AttachStderr: e.Stderr || e.Pty != nil,
		Tty: e.Pty != nil, Cmd: e.Argv, Env: env, WorkingDir: e.Cwd, User: e.User, ConsoleSize: size,
	})
	var hj *Hijacked
	tCreated := time.Now()
	if err == nil {
		hj, err = h.Docker.ExecStart(cctx, id, e.Pty != nil, size)
	}
	tStarted := time.Now()
	cancel()
	if err != nil {
		h.fail(s, err.Error())
		return
	}
	h.running.add(RunningExec{Tag: tag, Container: m.Container, ExecID: id, Pty: e.Pty != nil, Since: time.Now().UnixMilli()})
	// Without stdin attached, the connection's write side stays open: the
	// daemon takes a half-close as the client leaving, and stops copying the
	// output it hasn't sent yet.
	defer hj.Conn.Close()
	if err := s.Send(StreamMsg{OK: boolp(true)}); err != nil {
		h.hangUp(m, id, tag, e)
		return
	}

	// The exit code, as soon as the process exits, even while something it
	// started keeps its output open (npm run dev &).
	exited := make(chan int, 1)
	outDone := make(chan struct{})
	go func() {
		wait := 500 * time.Millisecond
		for {
			select {
			case <-outDone:
				wait = 5 * time.Millisecond
			case <-time.After(wait):
			}
			st, err := h.Docker.ExecInspect(context.Background(), id)
			if err == nil && !st.Running && st.ExitCode != nil {
				exited <- *st.ExitCode
				return
			}
			if err != nil && IsNotFound(err) {
				exited <- -1
				return
			}
			select {
			case <-outDone:
				wait = min(wait*2, 100*time.Millisecond)
			default:
			}
		}
	}()

	// Whether output is moving: being sent (perhaps waiting for credit), or
	// sent within the last outputIdle.
	var sending atomic.Bool
	var lastOut atomic.Int64
	send := func(ch int, p []byte) error {
		sending.Store(true)
		err := s.Data(ch, p)
		lastOut.Store(time.Now().UnixMilli())
		sending.Store(false)
		return err
	}
	idle := func() bool {
		return !sending.Load() && time.Since(time.UnixMilli(lastOut.Load())) > outputIdle && hj.Reader.Buffered() == 0
	}
	go func() {
		defer close(outDone)
		if e.Pty != nil {
			buf := make([]byte, frameMax)
			for {
				n, err := hj.Reader.Read(buf)
				if n > 0 && send(ChStdout, buf[:n]) != nil {
					h.debugf("exec %s: sending output failed", short(id))
					return
				}
				if err != nil {
					break
				}
			}
		} else if Demux(hj.Reader, func(ch int, p []byte) error {
			if ch == ChStderr && !e.Stderr {
				return nil
			}
			return send(ch, p)
		}) != nil {
			h.debugf("exec %s: sending output failed", short(id))
			return
		}
		s.Send(StreamMsg{EOF: intp(ChStdout)})
		if e.Stderr {
			s.Send(StreamMsg{EOF: intp(ChStderr)})
		}
	}()

	stdin := newStdinPump(s, hj.Conn, hj.CloseWrite)
	defer stdin.stop()
	reads := make(chan Inbound)
	readErr := make(chan error, 1)
	go func() {
		for {
			in, err := s.Read()
			if err != nil {
				readErr <- err
				return
			}
			select {
			case reads <- in:
			case <-s.Done():
				return
			}
		}
	}()
	exitSent := false
	var drained <-chan struct{} // outDone, once the exit is sent
	for {
		select {
		case code := <-exited:
			// The exit goes after all the output the process wrote, which may
			// still be on its way (or waiting for credit). Output that
			// something it started holds open (npm run dev &) has gone quiet,
			// or is cut off after exitWaitMax, and keeps flowing after the
			// exit until the object stops reading (its own grace period).
			for deadline := time.Now().Add(exitWaitMax); time.Now().Before(deadline); {
				select {
				case <-outDone:
					deadline = time.Now()
					continue
				case <-time.After(20 * time.Millisecond):
				}
				if idle() {
					break
				}
			}
			h.running.remove(tag)
			s.Send(StreamMsg{Exit: intp(code)})
			h.debugf("exec %s: exit %d sent; created in %v, started in %v, exit sent %v after the start", short(id), code,
				tCreated.Sub(t0).Round(time.Millisecond), tStarted.Sub(tCreated).Round(time.Millisecond), time.Since(tStarted).Round(time.Millisecond))
			exitSent, exited, drained = true, nil, outDone
		case <-drained:
			s.Close(1000, "exited")
			return
		case err := <-readErr:
			if exitSent {
				s.Abort()
				return
			}
			// The object went away before the process exited: hang up, as
			// a closed terminal or a client that left does.
			h.logf("exec %s: the stream ended before the process (%v); hanging up", short(id), err)
			s.Abort()
			h.hangUp(m, id, tag, e)
			return
		case in := <-reads:
			switch {
			case in.Ctl == nil:
				if in.Ch == ChStdin && e.Stdin {
					stdin.push(in.Data)
				}
			case in.Ctl.EOF != nil:
				stdin.eof()
			case in.Ctl.Resize != nil && e.Pty != nil:
				go h.Docker.ExecResize(context.Background(), id, in.Ctl.Resize.Cols, in.Ctl.Resize.Rows)
			case in.Ctl.Signal != nil:
				go h.signal(m, tag, *in.Ctl.Signal)
			}
		}
	}
}

// hangUp sends SIGHUP to a terminal's session and SIGTERM to any other, then
// SIGKILL if it's still there after killGrace, so a dropped stream leaves no
// process behind.
func (h *Handler) hangUp(m Target, id, tag string, e StreamExec) {
	sig := 15
	if e.Pty != nil {
		sig = 1
	}
	go func() {
		defer h.running.remove(tag)
		sid := h.signal(m, tag, sig)
		if sid == 0 {
			return
		}
		deadline := time.Now().Add(killGrace)
		for time.Now().Before(deadline) {
			st, err := h.Docker.ExecInspect(context.Background(), id)
			if err != nil || !st.Running {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		h.logf("exec %s: still running %v after the hang-up; killing it", short(id), killGrace)
		h.kill(m, sid, 9)
	}()
}

// sessionScript prints the pid of the process whose environment holds
// CX_EXEC=$1 and whose session id is its own pid: the exec's session leader.
// The fields after the ") " that ends the command name are state, ppid,
// pgrp and session.
const sessionScript = `for d in /proc/[0-9]*; do p=${d#/proc/}; ` +
	`tr '\0' '\n' < "$d/environ" 2>/dev/null | grep -qxF "CX_EXEC=$1" || continue; ` +
	`s=$(sed 's/.*) //' "$d/stat" 2>/dev/null | cut -d' ' -f4); [ "$s" = "$p" ] && echo "$p"; done; true`

// session finds an exec's session. Root can't read the environment of
// another user's process without CAP_SYS_PTRACE, which Docker drops, so it
// looks as root, then as the image's user.
func (h *Handler) session(m Target, tag string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, user := range []string{"0", "1000:1000"} {
		out, _, err := h.Docker.Run(ctx, m.Container, user, []string{"sh", "-c", sessionScript, "sh", tag})
		if err != nil {
			continue
		}
		if f := strings.Fields(out); len(f) > 0 {
			if pid, err := strconv.Atoi(f[0]); err == nil && pid > 1 {
				return pid
			}
		}
	}
	return 0
}

// signal sends sig to an exec's whole session, and says which it was (0 when
// it's gone).
func (h *Handler) signal(m Target, tag string, sig int) int {
	sid := h.session(m, tag)
	h.debugf("signal %d to the session of %s: %d", sig, tag, sid)
	if sid == 0 {
		return 0
	}
	h.kill(m, sid, sig)
	return sid
}

func (h *Handler) kill(m Target, sid, sig int) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// The image's sh is dash, whose kill takes no "--": kill -15 -917 signals group 917.
	h.Docker.Run(ctx, m.Container, "0", []string{"sh", "-c", `kill -"$1" -"$2"`, "sh", strconv.Itoa(sig), strconv.Itoa(sid)})
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// dialMachine connects to a port of the machine; a refusal or a timeout is
// "nothing listens there".
func dialMachine(ctx context.Context, m Target, port int) (net.Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	return d.DialContext(ctx, "tcp", net.JoinHostPort(m.Address, strconv.Itoa(port)))
}

// tcp connects the stream to a port: as framed data with credits, or with
// splice as the bytes themselves (ssh).
func (h *Handler) tcp(s *Stream, m Target, t StreamTCP) {
	conn, err := dialMachine(s.ctx, m, t.Port)
	if err != nil {
		s.Send(StreamMsg{Refused: strp(m.Address)})
		s.Close(1000, "refused")
		return
	}
	defer conn.Close()
	s.raw = t.Splice
	if err := s.Send(StreamMsg{OK: boolp(true)}); err != nil {
		return
	}
	closeWrite := func() error {
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			return cw.CloseWrite()
		}
		return nil
	}
	in := newStdinPump(s, conn, closeWrite)
	defer in.stop()
	machineDone := make(chan struct{})
	go func() {
		defer close(machineDone)
		buf := make([]byte, frameMax)
		for {
			n, err := conn.Read(buf)
			if n > 0 && s.Data(ChStdout, buf[:n]) != nil {
				return
			}
			if err != nil {
				return
			}
		}
	}()
	go func() {
		for {
			r, err := s.Read()
			if err != nil {
				conn.Close()
				return
			}
			switch {
			case r.Ctl == nil && (r.Ch == ChStdin || r.Ch < 0):
				in.push(r.Data)
			case r.Ctl != nil && r.Ctl.EOF != nil:
				in.eof()
			}
		}
	}()
	select {
	case <-machineDone:
		if !t.Splice {
			s.Send(StreamMsg{EOF: intp(ChStdout)})
		}
		s.Close(1000, "closed")
	case <-s.Done():
	}
}
