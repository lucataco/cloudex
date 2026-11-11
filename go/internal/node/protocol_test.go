package node

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The examples the TypeScript models are checked against too
// (packages/schema/test/node.test.ts).
type fixture struct {
	Protocol    int            `json:"protocol"`
	Close       map[string]int `json:"close"`
	StreamClose map[string]int `json:"streamClose"`
	Window      int            `json:"window"`
	Signing     struct {
		Method, Path, Sandbox, TS, Nonce, String, Seed, PublicKey, Signature string
	} `json:"signing"`
	Valid []struct {
		Name    string          `json:"name"`
		Model   string          `json:"model"`
		Message json.RawMessage `json:"message"`
	} `json:"valid"`
	Egress struct {
		Route            string `json:"route"`
		BodyMax          int64  `json:"bodyMax"`
		ActivityEverySec int    `json:"activityEverySec"`
		SavePartMax      int64  `json:"savePartMax"`
		Targets          struct {
			Valid   []string `json:"valid"`
			Invalid []string `json:"invalid"`
		} `json:"targets"`
	} `json:"egress"`
}

func loadFixture(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("../../../packages/schema/fixtures/node/protocol.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// decodeAs reads a message into the Go type its model stands for, refusing
// fields the type doesn't have.
func decodeAs(t *testing.T, model string, raw json.RawMessage) any {
	t.Helper()
	var head struct{ Type string }
	json.Unmarshal(raw, &head)
	var v any
	switch {
	case model == "NodeJoin":
		v = &Join{}
	case model == "NodeJoined":
		v = &Joined{}
	case model == "WorkerMessage" && head.Type == "tickets":
		v = &Tickets{}
	case model == "WorkerMessage" && head.Type == "welcome":
		v = &Welcome{}
	case model == "WorkerMessage":
		v = &Op{}
	case model == "StreamToAgent" || model == "StreamFromAgent":
		v = &StreamMsg{}
	case head.Type == "tickets":
		v = &TicketRequest{}
	case head.Type == "hello":
		v = &Hello{}
	case head.Type == "facts":
		v = &FactsMessage{}
	case head.Type == "state":
		v = &StateMessage{}
	case head.Type == "result":
		v = &Result{}
	default:
		t.Fatalf("no Go type for %s %s", model, head.Type)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		t.Fatalf("%s: %v", model, err)
	}
	return v
}

func sameJSON(t *testing.T, name string, a, b []byte) {
	t.Helper()
	var x, y any
	json.Unmarshal(a, &x)
	json.Unmarshal(b, &y)
	if !reflect.DeepEqual(x, y) {
		t.Errorf("%s: Go writes\n  %s\nthe fixture has\n  %s", name, a, b)
	}
}

func TestEveryExampleRoundTripsThroughTheGoTypes(t *testing.T) {
	f := loadFixture(t)
	if f.Protocol != Protocol {
		t.Fatalf("fixture protocol %d, Go %d", f.Protocol, Protocol)
	}
	want := map[string]int{"replaced": CloseReplaced, "revoked": CloseRevoked, "stale": CloseStale, "badMessage": CloseBadMessage}
	if !reflect.DeepEqual(f.Close, want) {
		t.Fatalf("close codes: fixture %v, Go %v", f.Close, want)
	}
	wantStream := map[string]int{"lost": StreamLost, "badTicket": StreamBadTicket, "badMessage": StreamBadMessage, "heartbeat": StreamHeartbeat}
	if !reflect.DeepEqual(f.StreamClose, wantStream) || f.Window != StreamWindow {
		t.Fatalf("stream close codes or window: fixture %v %d, Go %v %d", f.StreamClose, f.Window, wantStream, StreamWindow)
	}
	if len(f.Valid) < 8 {
		t.Fatalf("only %d examples", len(f.Valid))
	}
	for _, e := range f.Valid {
		v := decodeAs(t, e.Model, e.Message)
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		sameJSON(t, e.Name, out, e.Message)
	}
}

func example(t *testing.T, name string) json.RawMessage {
	for _, e := range loadFixture(t).Valid {
		if e.Name == name {
			return e.Message
		}
	}
	t.Fatalf("no example %s", name)
	return nil
}

// What the agent itself writes, built the way it builds it.
func TestTheAgentWritesTheMessagesInTheFixture(t *testing.T) {
	two, mem, disk := 2, 7936, 42.5
	hello := NewHello("v0.2.2", Facts{
		Arch: "amd64", Runtime: "docker", Isolation: Isolation{Firewall: "unchecked", Unisolated: true},
		CPUs: &two, MemoryMB: &mem, DiskFreeGB: &disk,
	}, []Machine{{ID: "cx-abc123", State: "running"}, {ID: "cx-def456", State: "stopped"}})
	b, _ := json.Marshal(hello)
	sameJSON(t, "hello", b, example(t, "hello"))

	empty := NewHello("dev", Facts{Arch: "arm64", Runtime: "docker", Isolation: Isolation{Firewall: "unchecked"}}, nil)
	b, _ = json.Marshal(empty)
	sameJSON(t, "hello-empty", b, example(t, "hello-empty"))

	a := &Agent{}
	b, _ = json.Marshal(a.answer(Op{Type: "op", ID: "op-k2j4h5", Name: "ping"}))
	sameJSON(t, "result-ok", b, example(t, "result-ok"))
	b, _ = json.Marshal(a.answer(Op{Type: "op", ID: "op-k2j4h6", Name: "exec"}))
	var r Result
	json.Unmarshal(b, &r)
	if r.OK || r.ID != "op-k2j4h6" || r.Error == "" {
		t.Fatalf("an unknown op: %s", b)
	}
}

func TestTheSigningExampleMatches(t *testing.T) {
	s := loadFixture(t).Signing
	if got := SigningString(s.Method, s.Path, s.Sandbox, s.TS, s.Nonce); got != s.String {
		t.Fatalf("signing string %q, fixture %q", got, s.String)
	}
	seed, _ := hex.DecodeString(s.Seed)
	key := ed25519.NewKeyFromSeed(seed)
	if PublicKey(key) != s.PublicKey {
		t.Fatalf("public key %s", PublicKey(key))
	}
	h := http.Header{}
	Signer{Key: key, Node: "nd-abcd1234", Now: func() time.Time { return time.Unix(1762819200, 0) }, Nonce: func() string { return s.Nonce }}.
		Sign(h, s.Method, s.Path, s.Sandbox)
	if h.Get(HeaderSig) != s.Signature {
		t.Fatalf("signature %s, fixture %s", h.Get(HeaderSig), s.Signature)
	}
	if h.Get(HeaderNode) != "nd-abcd1234" || h.Get(HeaderTS) != s.TS || h.Get(HeaderSandbox) != "" {
		t.Fatalf("headers %v", h)
	}
	sig, _ := base64.StdEncoding.DecodeString(h.Get(HeaderSig))
	if !ed25519.Verify(key.Public().(ed25519.PublicKey), []byte(s.String), sig) {
		t.Fatal("doesn't verify")
	}
}

func TestEndpointSignsThePathAsSent(t *testing.T) {
	for api, want := range map[string]string{
		"https://cloudex-api.x.workers.dev":  "/v1/node/connect",
		"https://cloudex-api.x.workers.dev/": "/v1/node/connect",
		"http://127.0.0.1:8787":              "/v1/node/connect",
		"https://example.com/base?region=eu": "/base/v1/node/connect?region=eu",
	} {
		_, signed, err := Endpoint(api, "/v1/node/connect")
		if err != nil || signed != want {
			t.Errorf("%s: %q, %v; want %q", api, signed, err, want)
		}
	}
	for _, bad := range []string{"", "cloudex-api.x.workers.dev", "ftp://x", "https://"} {
		if _, _, err := Endpoint(bad, "/v1/node/connect"); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The route the agent passes its machines' requests on (M6), its limits, and
// the targets it sends, as the Worker reads them (nodeegress.ts).
func TestEgressMatchesTheFixture(t *testing.T) {
	f := loadFixture(t)
	e := f.Egress
	if e.Route != EgressRoute || e.BodyMax != EgressBodyMax || time.Duration(e.ActivityEverySec)*time.Second != ActivityEvery {
		t.Fatalf("fixture %s %d %ds, Go %s %d %v", e.Route, e.BodyMax, e.ActivityEverySec, EgressRoute, EgressBodyMax, ActivityEvery)
	}
	if e.SavePartMax > EgressBodyMax {
		t.Fatalf("save parts of %d don't fit under %d", e.SavePartMax, EgressBodyMax)
	}
	for _, target := range e.Targets.Valid {
		full, signed, err := EgressURL("https://api.example/", target)
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(full)
		if u.Query().Get("target") != target || u.Path != EgressRoute || signed != u.EscapedPath()+"?"+u.RawQuery {
			t.Fatalf("%s: %s signed as %s", target, full, signed)
		}
	}
	// What the welcome names is what the CA is limited to.
	for _, ex := range f.Valid {
		if ex.Model == "WorkerMessage" && strings.Contains(string(ex.Message), `"providers"`) {
			var w Welcome
			json.Unmarshal(ex.Message, &w)
			if _, err := CleanHosts(w.Providers); err != nil {
				t.Fatalf("%s: %v", ex.Name, err)
			}
		}
	}
}
