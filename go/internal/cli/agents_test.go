package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
)

// printEvents renders a file of events, one JSON object per line, as the
// agents commands print them.
func printEvents(t *testing.T, agent, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out bytes.Buffer
	p := newAgentPrinter(&out, agent)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e api.AgentEvent
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if err := p.print(e); err != nil {
			t.Fatal(err)
		}
	}
	return out.String()
}

// The fixtures are real logs: Claude Code and Codex in a sandbox, talking to a
// stand-in model API that asks each to run one command.
func TestAgentPrinterReadsClaudeCode(t *testing.T) {
	want := `> please run: echo hello from claude && ls ~ | head -3
I'll run that.
$ echo hello from claude && ls ~ | head -3
  hello from claude
  mock.py
The command ran.
-- turn 1 done in 4s
`
	if got := printEvents(t, "claude", "testdata/claude-events.jsonl"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAgentPrinterReadsCodex(t *testing.T) {
	want := "> please run: echo hello from codex && false\n" +
		"error: Model metadata for `gpt-5` not found. Defaulting to fallback metadata; this can degrade performance and cause issues.\n" +
		`$ echo hello from codex && false
  hello from codex
  exit 1
The command ran.
-- turn 1 done in 3s
`
	if got := printEvents(t, "codex", "testdata/codex-events.jsonl"); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestAgentPrinterDetails(t *testing.T) {
	var out bytes.Buffer
	p := newAgentPrinter(&out, "claude")
	ev := func(e api.AgentEvent, raw string) {
		t.Helper()
		e.Event = json.RawMessage(raw)
		if err := p.print(e); err != nil {
			t.Fatal(err)
		}
	}
	errText := "out of credit"
	ev(api.AgentEvent{Turn: 2, At: 1000, Type: "prompt", Text: "first line\nsecond line"}, "")
	ev(api.AgentEvent{Turn: 2, Type: "event"}, `{"type":"assistant","message":{"content":[
		{"type":"thinking","thinking":"hidden"},
		{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/home/cloudex/app/main.go"}},
		{"type":"tool_use","id":"t2","name":"Bash","input":{"command":"go test ./..."}}]}}`)
	ev(api.AgentEvent{Turn: 2, Type: "event"}, `{"type":"user","message":{"content":[
		{"type":"tool_result","tool_use_id":"t1","content":"package main"},
		{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"a\nb\nc\nd\ne\nok"}]}]}}`)
	ev(api.AgentEvent{Turn: 2, Type: "event"}, `{"type":"assistant","message":{"content":[
		{"type":"tool_use","id":"t3","name":"Bash","input":{"command":"make"}},
		{"type":"tool_use","id":"t4","name":"Edit","input":{"file_path":"/home/cloudex/app/x.go"}}]}}`)
	ev(api.AgentEvent{Turn: 2, Type: "event"}, `{"type":"user","message":{"content":[
		{"type":"tool_result","tool_use_id":"t3","is_error":true,"content":"Exit code 2\nmake: *** [all] Error 2"},
		{"type":"tool_result","tool_use_id":"t4","is_error":true,"content":"File has not been read yet."}]}}`)
	ev(api.AgentEvent{Turn: 2, Type: "output", Text: "a long line", Truncated: true}, "")
	ev(api.AgentEvent{Turn: 2, Type: "event"}, `{"type":"result","is_error":true,"result":"out of credit","total_cost_usd":0.25}`)
	ev(api.AgentEvent{Turn: 2, At: 63_000, Type: "end", Error: &errText}, "")
	ev(api.AgentEvent{Turn: 3, At: 70_000, Type: "end", Interrupted: true}, "")

	want := `
> first line
  second line
Read /home/cloudex/app/main.go
$ go test ./...
  … 3 more lines
  d
  e
  ok
$ make
Edit /home/cloudex/app/x.go
  make: *** [all] Error 2
  exit 2
  error: File has not been read yet.
a long line [cut]
error: out of credit
-- turn 2 failed in 1m2s, $0.25: out of credit
-- turn 3 interrupted
`
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
}

// fakeAgents serves one sandbox's agents and records each request's URL.
type fakeAgents struct {
	mu      sync.Mutex
	urls    []string
	bodies  []map[string]any
	list    string
	events  []string // answers to successive events requests
	answers map[string]string
}

func (f *fakeAgents) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.urls = append(f.urls, r.Method+" "+r.URL.RequestURI())
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	f.bodies = append(f.bodies, body)
	w.Header().Set("Content-Type", "application/json")
	key := r.Method + " " + r.URL.Path
	switch {
	case strings.HasSuffix(r.URL.Path, "/events"):
		if len(f.events) == 0 {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":"no more events"}`))
			return
		}
		w.Write([]byte(f.events[0]))
		f.events = f.events[1:]
	case key == "GET /v1/sandboxes/cx-abc123/agents":
		w.Write([]byte(f.list))
	default:
		if a, ok := f.answers[key]; ok {
			w.Write([]byte(a))
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"no route ` + key + `"}`))
	}
}

func newFakeAgents(t *testing.T, f *fakeAgents) {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	if err := (&config.Config{Current: "cx-abc123"}).Save(); err != nil {
		t.Fatal(err)
	}
}

func agentJSON(id, state string, events, updated int, exit string) string {
	return fmt.Sprintf(`{"id":%q,"agent":"claude","state":%q,"cwd":"/home/cloudex","model":null,"turns":1,"queued":0,
"events":%d,"sessionId":null,"lastMessage":"All done.\nMore.","lastExitCode":%s,"error":null,"createdAt":%d,"updatedAt":%d}`,
		id, state, events, exit, updated, updated)
}

func TestAgentsStartFollowsUntilIdle(t *testing.T) {
	f := &fakeAgents{
		answers: map[string]string{"POST /v1/sandboxes/cx-abc123/agents": agentJSON("ag-aaaaaaaa", "running", 0, 1, "null")},
		events: []string{
			`{"events":[{"seq":1,"turn":1,"at":0,"type":"prompt","text":"fix it"}],"next":1,"agent":` + agentJSON("ag-aaaaaaaa", "running", 1, 1, "null") + `}`,
			`{"events":[],"next":1,"agent":` + agentJSON("ag-aaaaaaaa", "running", 1, 1, "null") + `}`,
			`{"events":[{"seq":2,"turn":1,"at":2000,"type":"end","exitCode":2}],"next":2,"agent":` + agentJSON("ag-aaaaaaaa", "idle", 2, 1, "2") + `}`,
		},
	}
	newFakeAgents(t, f)
	code, out, _ := run(t, "agents", "start", "--agent", "codex", "--model", "gpt-5", "fix it")
	if code != 2 {
		t.Fatalf("exit %d, want the turn's 2", code)
	}
	if out != "> fix it\n-- turn 1 failed in 2s (exit 2)\n" {
		t.Fatalf("output %q", out)
	}
	if b := f.bodies[0]; b["prompt"] != "fix it" || b["agent"] != "codex" || b["model"] != "gpt-5" {
		t.Fatalf("start body %v", b)
	}
	want := []string{"/events?after=0&wait=30", "/events?after=1&wait=30", "/events?after=1&wait=30"}
	for i, w := range want {
		if !strings.HasSuffix(f.urls[i+1], w) {
			t.Fatalf("request %d is %s, want ...%s", i+1, f.urls[i+1], w)
		}
	}
}

func TestAgentsLogsUsesTheMostRecentAgent(t *testing.T) {
	f := &fakeAgents{
		list: "[" + agentJSON("ag-older000", "idle", 1, 5, "0") + "," + agentJSON("ag-newer000", "idle", 1, 9, "0") + "]",
		events: []string{
			`{"events":[{"seq":1,"turn":1,"at":0,"type":"prompt","text":"hi"}],"next":1,"agent":` + agentJSON("ag-newer000", "idle", 1, 9, "0") + `}`,
		},
	}
	newFakeAgents(t, f)
	code, out, _ := run(t, "agents", "logs")
	if code != 0 || out != "> hi\n" {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if !strings.Contains(f.urls[1], "/agents/ag-newer000/events?after=0&wait=0") {
		t.Fatalf("asked for %s", f.urls[1])
	}
}

func TestAgentsSendFollowsFromTheNewMessage(t *testing.T) {
	f := &fakeAgents{
		answers: map[string]string{
			"GET /v1/sandboxes/web/agents/ag-aaaaaaaa":           agentJSON("ag-aaaaaaaa", "idle", 7, 1, "0"),
			"POST /v1/sandboxes/web/agents/ag-aaaaaaaa/messages": agentJSON("ag-aaaaaaaa", "running", 7, 1, "0"),
		},
		events: []string{
			`{"events":[{"seq":8,"turn":2,"at":0,"type":"prompt","text":"more"},{"seq":9,"turn":2,"at":0,"type":"end","exitCode":0}],"next":9,"agent":` +
				agentJSON("ag-aaaaaaaa", "idle", 9, 1, "0") + `}`,
		},
	}
	newFakeAgents(t, f)
	code, out, _ := run(t, "agents", "send", "--interrupt", "web", "ag-aaaaaaaa", "more")
	if code != 0 || out != "\n> more\n-- turn 2 done in 0s\n" {
		t.Fatalf("exit %d, output %q", code, out)
	}
	if b := f.bodies[1]; b["prompt"] != "more" || b["interrupt"] != true {
		t.Fatalf("message body %v", b)
	}
	if !strings.HasSuffix(f.urls[2], "/events?after=7&wait=30") {
		t.Fatalf("followed from %s", f.urls[2])
	}
}

func TestAgentsListAndArguments(t *testing.T) {
	f := &fakeAgents{list: "[" + agentJSON("ag-aaaaaaaa", "running", 3, 1, "null") + "]"}
	newFakeAgents(t, f)
	code, out, _ := run(t, "agents")
	if code != 0 || !strings.Contains(out, "ag-aaaaaaaa  claude  running  1") || !strings.Contains(out, "All done. …") {
		t.Fatalf("exit %d, list:\n%s", code, out)
	}
	if code, _, errOut := run(t, "agents", "logs", "web", "not-an-agent"); code != 1 || !strings.Contains(errOut, "isn't an agent id") {
		t.Fatalf("bad agent id: exit %d, %q", code, errOut)
	}
	if code, _, errOut := run(t, "agents", "start", "--agent", "gemini", "x"); code != 1 || !strings.Contains(errOut, "unknown agent") {
		t.Fatalf("bad agent: exit %d, %q", code, errOut)
	}
}

func TestOneLine(t *testing.T) {
	for in, want := range map[string]string{
		"short":          "short",
		"two\nlines":     "two …",
		"  padded  ":     "padded",
		"0123456789abcd": "012345678…",
	} {
		if got := oneLine(in, 10); got != want {
			t.Errorf("oneLine(%q) = %q, want %q", in, got, want)
		}
	}
}
