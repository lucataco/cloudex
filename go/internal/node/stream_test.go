package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func muxFrame(stream byte, p string) []byte {
	h := make([]byte, 8)
	h[0] = stream
	binary.BigEndian.PutUint32(h[4:], uint32(len(p)))
	return append(h, p...)
}

func TestDemuxSplitsChannelsAndCoalescesWhatIsBuffered(t *testing.T) {
	var in bytes.Buffer
	for _, f := range [][]byte{muxFrame(1, "a"), muxFrame(1, "b"), muxFrame(2, "E"), muxFrame(1, "c")} {
		in.Write(f)
	}
	type got struct {
		ch int
		p  string
	}
	var out []got
	err := Demux(bufio.NewReader(&in), func(ch int, p []byte) error {
		out = append(out, got{ch, string(p)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []got{{ChStdout, "ab"}, {ChStderr, "E"}, {ChStdout, "c"}}
	if fmt.Sprint(out) != fmt.Sprint(want) {
		t.Fatalf("got %v, want %v", out, want)
	}
}

func TestDemuxKeepsFramesUnder256KiB(t *testing.T) {
	var in bytes.Buffer
	big := strings.Repeat("x", 200<<10)
	in.Write(muxFrame(1, big))
	in.Write(muxFrame(1, big))
	total := 0
	Demux(bufio.NewReader(&in), func(ch int, p []byte) error {
		if len(p) > 256<<10 {
			t.Fatalf("a frame of %d bytes", len(p))
		}
		total += len(p)
		return nil
	})
	if total != 400<<10 {
		t.Fatalf("got %d bytes", total)
	}
}

func TestReadTicketTakesTheStreamAndExpiry(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"n":"nd-abcd1234","s":"cx-abc123","i":"0123456789abcdef","e":1762819260}`))
	tk, err := readTicket(payload + ".bWFj")
	if err != nil || tk.stream != "0123456789abcdef" || tk.expires.Unix() != 1762819260 {
		t.Fatalf("%+v %v", tk, err)
	}
	for _, bad := range []string{"", "nodot", "!!!.mac", base64.RawURLEncoding.EncodeToString([]byte(`{"e":1}`)) + ".m"} {
		if _, err := readTicket(bad); err == nil {
			t.Errorf("%q read as a ticket", bad)
		}
	}
}

func TestWindowWaitsForCredit(t *testing.T) {
	w := newWindow(10)
	if n, _ := w.take(25); n != 10 {
		t.Fatalf("took %d", n)
	}
	done := make(chan int)
	go func() {
		n, _ := w.take(5)
		done <- n
	}()
	select {
	case <-done:
		t.Fatal("took without credit")
	case <-time.After(20 * time.Millisecond):
	}
	w.grant(3)
	if n := <-done; n != 3 {
		t.Fatalf("took %d", n)
	}
	go func() {
		_, err := w.take(1)
		done <- map[bool]int{true: -1, false: 0}[err != nil]
	}()
	w.kill()
	if <-done != -1 {
		t.Fatal("a dead window still hands out credit")
	}
}

// fakeDocker answers the Engine API calls an exec makes, over a unix socket.
// Its commands: ["echo", text, n] writes text n times to stdout, "E" to
// stderr, and exits 3; ["cat"] copies stdin to stdout and exits 0.
type fakeDocker struct {
	mu        sync.Mutex
	cfg       ExecConfig
	running   bool
	code      int
	halfClose bool // whether the client closed its write side
}

func startFakeDocker(t *testing.T) (*fakeDocker, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cxd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeDocker{}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return f, sock
}

func (f *fakeDocker) serve(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/exec"):
		f.mu.Lock()
		json.NewDecoder(r.Body).Decode(&f.cfg)
		f.running = true
		f.mu.Unlock()
		w.Write([]byte(`{"Id":"e1"}`))
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/start"):
		io.ReadAll(r.Body) // the start's own body comes before the stream
		conn, brw, _ := w.(http.Hijacker).Hijack()
		brw.WriteString("HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		brw.Flush()
		go f.run(conn, brw.Reader)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/json"):
		f.mu.Lock()
		defer f.mu.Unlock()
		fmt.Fprintf(w, `{"Running":%v,"ExitCode":%d,"Pid":1}`, f.running, f.code)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeDocker) run(conn net.Conn, in *bufio.Reader) {
	defer conn.Close()
	f.mu.Lock()
	cmd := f.cfg.Cmd
	f.mu.Unlock()
	code := 0
	switch cmd[0] {
	case "echo":
		var n int
		fmt.Sscan(cmd[2], &n)
		for i := 0; i < n; i++ {
			conn.Write(muxFrame(1, cmd[1]))
		}
		conn.Write(muxFrame(2, "E"))
		code = 3
		// Notice a half-close of stdin, which must not happen without stdin.
		conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		if _, err := in.ReadByte(); err == io.EOF {
			f.mu.Lock()
			f.halfClose = true
			f.mu.Unlock()
		}
	case "yes": // prints forever, as a planted sh might (review H1)
		line := strings.Repeat("y", 4095) + "\n"
		for {
			if _, err := conn.Write(muxFrame(1, line)); err != nil {
				break
			}
		}
	case "hang": // never exits and never prints
		in.ReadByte()
	case "cat":
		buf := make([]byte, 64<<10)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				conn.Write(muxFrame(1, string(buf[:n])))
			}
			if err != nil {
				break
			}
		}
	}
	f.mu.Lock()
	f.running, f.code = false, code
	f.mu.Unlock()
}

// object plays the Sandbox object's half of one stream socket.
type object struct {
	c    *websocket.Conn
	ctx  context.Context
	out  map[int]*bytes.Buffer
	ctl  []StreamMsg
	exit *int
}

func (o *object) read(t *testing.T, credit bool) {
	t.Helper()
	for o.exit == nil {
		typ, b, err := o.c.Read(o.ctx)
		if err != nil {
			t.Fatalf("the stream ended before the exit: %v", err)
		}
		if typ == websocket.MessageBinary {
			o.out[int(b[0])].Write(b[1:])
			if credit {
				m, _ := json.Marshal(StreamMsg{Credit: &Credit{Ch: int(b[0]), N: len(b) - 1}})
				o.c.Write(o.ctx, websocket.MessageText, m)
			}
			continue
		}
		if string(b) == HB {
			continue
		}
		var m StreamMsg
		json.Unmarshal(b, &m)
		o.ctl = append(o.ctl, m)
		if m.Exit != nil {
			o.exit = m.Exit
		}
	}
}

// pairStream connects an agent-side Stream to an object-side socket.
func pairStream(t *testing.T) (*Stream, *object) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		c.SetReadLimit(1 << 24)
		accepted <- c
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(1 << 24)
	s := newStream(ctx, "0123456789abcdef", c)
	o := &object{c: <-accepted, ctx: ctx, out: map[int]*bytes.Buffer{0: {}, 1: {}, 2: {}, 3: {}, 4: {}}}
	return s, o
}

func TestAnExecSendsItsOutputThenItsExitWithinTheWindow(t *testing.T) {
	d, sock := startFakeDocker(t)
	s, o := pairStream(t)
	h := &Handler{Docker: &Docker{Socket: sock}}
	// 6 MiB of output: more than one window, so it needs credit to finish.
	go h.Serve(s, Target{Container: "c1"}, StreamMsg{Exec: &StreamExec{Argv: []string{"echo", strings.Repeat("y", 64<<10), "96"}, Stdin: false, Stderr: true}})
	o.read(t, true)
	if *o.exit != 3 || o.out[1].Len() != 6<<20 || o.out[2].String() != "E" {
		t.Fatalf("exit %d, stdout %d bytes, stderr %q", *o.exit, o.out[1].Len(), o.out[2].String())
	}
	// ok first, both eofs before the exit.
	var order []string
	for _, m := range o.ctl {
		switch {
		case m.OK != nil:
			order = append(order, "ok")
		case m.EOF != nil:
			order = append(order, fmt.Sprintf("eof%d", *m.EOF))
		case m.Exit != nil:
			order = append(order, "exit")
		}
	}
	if strings.Join(order, " ") != "ok eof1 eof2 exit" {
		t.Fatalf("messages in the order %v", order)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.halfClose {
		t.Fatal("stdin, which wasn't attached, was half-closed: the daemon stops copying output then")
	}
	if !strings.HasPrefix(d.cfg.Env[len(d.cfg.Env)-1], "CX_EXEC=") || d.cfg.AttachStdin {
		t.Fatalf("exec config %+v", d.cfg)
	}
}

func TestWithoutCreditAnExecStopsAtTheWindow(t *testing.T) {
	_, sock := startFakeDocker(t)
	s, o := pairStream(t)
	h := &Handler{Docker: &Docker{Socket: sock}}
	go h.Serve(s, Target{Container: "c1"}, StreamMsg{Exec: &StreamExec{Argv: []string{"echo", strings.Repeat("y", 64<<10), "96"}, Stderr: true}})
	got := 0
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		rctx, cancel := context.WithDeadline(o.ctx, deadline)
		typ, b, err := o.c.Read(rctx)
		cancel()
		if err != nil {
			break
		}
		if typ == websocket.MessageBinary {
			got += len(b) - 1
		}
	}
	if got != StreamWindow {
		t.Fatalf("sent %d bytes without credit, want the window, %d", got, StreamWindow)
	}
}

func TestStdinGoesInAndIsCredited(t *testing.T) {
	_, sock := startFakeDocker(t)
	s, o := pairStream(t)
	h := &Handler{Docker: &Docker{Socket: sock}}
	go h.Serve(s, Target{Container: "c1"}, StreamMsg{Exec: &StreamExec{Argv: []string{"cat"}, Stdin: true, Stderr: true}})
	in := bytes.Repeat([]byte("0123456789abcdef"), (2<<20)/16)
	for p := in; len(p) > 0; {
		n := min(len(p), 128<<10)
		o.c.Write(o.ctx, websocket.MessageBinary, append([]byte{ChStdin}, p[:n]...))
		p = p[n:]
	}
	eof, _ := json.Marshal(StreamMsg{EOF: intp(0)})
	o.c.Write(o.ctx, websocket.MessageText, eof)
	o.read(t, true)
	if !bytes.Equal(o.out[1].Bytes(), in) || *o.exit != 0 {
		t.Fatalf("got %d bytes back, exit %d", o.out[1].Len(), *o.exit)
	}
	credited := 0
	for _, m := range o.ctl {
		if m.Credit != nil && m.Credit.Ch == ChStdin {
			credited += m.Credit.N
		}
	}
	if credited < len(in)-StreamWindow/4 {
		t.Fatalf("stdin credited %d of %d bytes", credited, len(in))
	}
}

func TestTheSessionScriptFindsOnlyTheSessionLeader(t *testing.T) {
	// The script runs in the machine; here, its pieces: the tag is matched
	// whole, and the session id is the fourth field after the command name.
	if !strings.Contains(sessionScript, `grep -qxF "CX_EXEC=$1"`) || !strings.Contains(sessionScript, `cut -d' ' -f4`) {
		t.Fatal(sessionScript)
	}
}
