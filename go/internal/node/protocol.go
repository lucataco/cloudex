// Package node is cloudex-node, the agent that runs on a user's own host (a
// node) and keeps a control WebSocket open to the deployment's Node Durable
// Object (apps/api/src/node.ts). The host has no inbound port: the agent dials
// out, signs every request with the node's own Ed25519 key, and reconnects by
// itself after a drop or a deploy.
//
// The messages are defined in packages/schema/src/node.ts. The examples in
// packages/schema/fixtures/node/protocol.json are what both sides are checked
// against (protocol_test.go here), so a field renamed on one side alone fails
// a test.
package node

import "encoding/json"

// Protocol is the control protocol's version; the Worker refuses another.
const Protocol = 1

// Close codes the Worker sends on the control socket.
const (
	CloseReplaced   = 4000 // a newer connection with this node's key took over
	CloseRevoked    = 4003 // cloudex node rm: the key no longer works
	CloseStale      = 4008 // the Worker heard no ping for a minute
	CloseBadMessage = 4400
)

// Isolation is what the agent knows about how its machines are kept apart.
type Isolation struct {
	Firewall   string `json:"firewall"` // pass, fail or unchecked
	Unisolated bool   `json:"unisolated"`
}

// Facts describe the host. Sizes it can't tell are null.
type Facts struct {
	Arch       string    `json:"arch"`    // amd64 or arm64
	Runtime    string    `json:"runtime"` // docker or sysbox
	Isolation  Isolation `json:"isolation"`
	CPUs       *int      `json:"cpus"`
	MemoryMB   *int      `json:"memoryMb"`
	DiskFreeGB *float64  `json:"diskFreeGb"`
}

// Machine is a sandbox's container on this node, as the agent sees it.
type Machine struct {
	ID    string `json:"id"`
	State string `json:"state"` // running or stopped
}

// Join is the body of POST /v1/node/join.
type Join struct {
	Token     string `json:"token"`
	PublicKey string `json:"publicKey"`
	Version   string `json:"version"`
	Facts     Facts  `json:"facts"`
}

// Joined is its answer.
type Joined struct {
	Node struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"node"`
}

// Hello is the agent's first message on every connection.
type Hello struct {
	Type     string    `json:"type"` // hello
	Protocol int       `json:"protocol"`
	Version  string    `json:"version"`
	Facts    Facts     `json:"facts"`
	Machines []Machine `json:"machines"`
}

// FactsMessage reports facts that changed.
type FactsMessage struct {
	Type  string `json:"type"` // facts
	Facts Facts  `json:"facts"`
}

// StateMessage reports a machine whose state changed.
type StateMessage struct {
	Type    string  `json:"type"` // state
	Machine Machine `json:"machine"`
}

// Result answers an op: Value when OK, else Error.
type Result struct {
	Type  string          `json:"type"` // result
	ID    string          `json:"id"`
	OK    bool            `json:"ok"`
	Value json.RawMessage `json:"value,omitempty"`
	Error string          `json:"error,omitempty"`
}

// Op is a request from the Worker. The set of names is fixed, and none of
// them runs anything on the host.
type Op struct {
	Type string                     `json:"type"` // op
	ID   string                     `json:"id"`
	Name string                     `json:"name"`
	Args map[string]json.RawMessage `json:"args"`
}

// NewHello is the hello the agent sends.
func NewHello(version string, f Facts, machines []Machine) Hello {
	if machines == nil {
		machines = []Machine{}
	}
	return Hello{Type: "hello", Protocol: Protocol, Version: version, Facts: f, Machines: machines}
}

func okResult(id string, value any) Result {
	b, err := json.Marshal(value)
	if err != nil {
		return errResult(id, err.Error())
	}
	return Result{Type: "result", ID: id, OK: true, Value: b}
}

func errResult(id, msg string) Result {
	if len(msg) > 2000 {
		msg = msg[:2000]
	}
	return Result{Type: "result", ID: id, OK: false, Error: msg}
}

// TicketRequest asks the Node object for stream tickets for one sandbox (M3).
type TicketRequest struct {
	Type    string `json:"type"` // tickets
	ID      string `json:"id"`   // tk-...
	Sandbox string `json:"sandbox"`
	Count   int    `json:"count"`
}

// Tickets is the answer: each one opens one stream socket, once, within 60 s.
type Tickets struct {
	Type    string   `json:"type"` // tickets
	ID      string   `json:"id"`
	Tickets []string `json:"tickets"`
	Error   string   `json:"error,omitempty"`
}

// StreamTicketsMax is the most tickets one request gets.
const StreamTicketsMax = 8

// ---------------------------------------------------------------- streams
//
// A stream socket carries one exec, TCP connection or HTTP exchange between
// the agent and a sandbox's object (packages/schema/src/node.ts, "streams").
// Binary frames start with a channel byte; text frames are the JSON control
// messages below, one key each, or the heartbeat.

// StreamWindow is how many bytes a sender may have in flight on a channel
// before the receiver grants more with a credit message.
const StreamWindow = 4 << 20

// Channels of binary frames.
const (
	ChStdin   = 0 // stdin, a request body, or a binary message to the machine
	ChStdout  = 1 // stdout, a response body, or a binary message from the machine
	ChStderr  = 2
	ChTextIn  = 3 // a preview WebSocket's text message to the machine
	ChTextOut = 4 // and from it
)

// Close codes on a stream socket.
const (
	StreamLost       = 4404
	StreamBadTicket  = 4401
	StreamBadMessage = 4400
	StreamHeartbeat  = 4408
)

type Pty struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type StreamExec struct {
	Argv   []string          `json:"argv"`
	Cwd    string            `json:"cwd,omitempty"`
	Env    map[string]string `json:"env,omitempty"`
	User   string            `json:"user,omitempty"`
	Pty    *Pty              `json:"pty,omitempty"`
	Stdin  bool              `json:"stdin"`
	Stderr bool              `json:"stderr"`
}

type StreamTCP struct {
	Port   int  `json:"port"`
	Splice bool `json:"splice"`
}

type StreamHTTP struct {
	Port    int         `json:"port"`
	Method  string      `json:"method"`
	Path    string      `json:"path"`
	Headers [][2]string `json:"headers"`
	Body    bool        `json:"body"`
}

type Credit struct {
	Ch int `json:"ch"`
	N  int `json:"n"`
}

type WSClose struct {
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

type HTTPStatus struct {
	Code    int         `json:"code"`
	Headers [][2]string `json:"headers"`
}

// StreamMsg is any control message on a stream, in either direction: exactly
// one field is set.
type StreamMsg struct {
	Exec   *StreamExec `json:"exec,omitempty"`
	TCP    *StreamTCP  `json:"tcp,omitempty"`
	HTTP   *StreamHTTP `json:"http,omitempty"`
	Resize *Pty        `json:"resize,omitempty"`
	Signal *int        `json:"signal,omitempty"`
	EOF    *int        `json:"eof,omitempty"`
	Credit *Credit     `json:"credit,omitempty"`
	Close  *WSClose    `json:"close,omitempty"`

	OK      *bool       `json:"ok,omitempty"`
	Refused *string     `json:"refused,omitempty"`
	Error   *string     `json:"error,omitempty"`
	Status  *HTTPStatus `json:"status,omitempty"`
	Exit    *int        `json:"exit,omitempty"`
}

func intp(v int) *int       { return &v }
func strp(v string) *string { return &v }
func boolp(v bool) *bool    { return &v }
