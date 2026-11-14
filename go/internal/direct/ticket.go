// Package direct holds what the agent (go/internal/node) and the CLI share
// for the direct path (v0.2.3): ssh and other TCP connections from a client
// straight to a node's agent over the LAN or a tailnet. The protocol is in
// packages/schema/src/node.ts ("the direct path"); the Worker's side is
// apps/api/src/direct.ts. Both are checked against the vectors in
// packages/schema/fixtures/node/protocol.json ("direct").
//
// This file has the tickets. The Sandbox object mints one for one node,
// sandbox, port and session, good for TicketTTL; the agent checks it with the
// node's secret, which the Worker sends in welcome.
package direct

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// ALPN is the protocol the TLS handshake on the direct port must agree on.
	ALPN = "cloudex-direct/1"
	// Port is the direct port's default.
	Port = 7022
	// TicketTTL is how long a ticket is good for after it's minted.
	TicketTTL = 60 * time.Second
	// TicketMax is the longest ticket, in bytes.
	TicketMax = 512
	// SessionsMax is the most sessions one sandbox may have open on the direct
	// port, and the most a report names.
	SessionsMax = 64
	// ReportEvery is how often the agent reports a sandbox's open sessions
	// while any is open.
	ReportEvery = 30 * time.Second
	// Label is what the Worker derives the direct secrets' key from ADMIN_KEY with.
	Label = "cloudex node direct tickets v1"
)

// Claims are what a ticket says: one connection to Port on Sandbox, on Node,
// for the session Session, until Exp (unix seconds).
type Claims struct {
	Node    string `json:"n"`
	Sandbox string `json:"s"`
	Port    int    `json:"p"`
	Session string `json:"i"`
	Exp     int64  `json:"e"`
}

var (
	nodeID    = regexp.MustCompile(`^nd-[a-z0-9]{8}$`)
	sandboxID = regexp.MustCompile(`^cx-[a-z0-9]{6}$`)
	sessionID = regexp.MustCompile(`^[0-9a-f]{16}$`)
	b64url    = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// Errors Verify gives; the agent answers "err <reason>" with their text.
var (
	ErrTooLong   = errors.New("ticket too long")
	ErrMalformed = errors.New("malformed ticket")
	ErrBadMAC    = errors.New("bad ticket")
	ErrClaims    = errors.New("malformed claims")
)

// Secret is the node's secret as the Worker derives it (direct.ts
// directSecret). The agent never knows ADMIN_KEY: this is for tests.
func Secret(adminKey, node, publicKey string) []byte {
	root := hmac.New(sha256.New, []byte(adminKey))
	root.Write([]byte(Label))
	m := hmac.New(sha256.New, root.Sum(nil))
	m.Write([]byte(node + "\n" + publicKey))
	return m.Sum(nil)
}

// DecodeSecret reads the secret as welcome carries it: 32 bytes in base64.
func DecodeSecret(s string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != 32 {
		return nil, errors.New("the direct secret isn't 32 bytes in base64")
	}
	return b, nil
}

// NewSession is a random session id: 16 hex digits.
func NewSession() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func mac(secret []byte, payload string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(payload))
	return m.Sum(nil)
}

// Mint makes a ticket as the Worker does (direct.ts mintDirectTicket), with
// c.Exp as given. For tests and probes: only the deployment mints real ones.
func Mint(secret []byte, c Claims) string {
	b, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(b)
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac(secret, payload))
}

// Verify checks a ticket's MAC under secret and the form of its claims, and
// returns them. Expiry, the node, the sandbox and replay are the caller's to
// check (Claims.Expired), since only it knows the Worker's clock and what runs.
func Verify(secret []byte, ticket string) (Claims, error) {
	var c Claims
	if len(ticket) > TicketMax {
		return c, ErrTooLong
	}
	payload, sig, ok := strings.Cut(ticket, ".")
	if !ok || payload == "" || sig == "" || strings.Contains(sig, ".") || !b64url.MatchString(payload) || !b64url.MatchString(sig) {
		return c, ErrMalformed
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return c, ErrMalformed
	}
	if !hmac.Equal(got, mac(secret, payload)) {
		return c, ErrBadMAC
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return c, ErrMalformed
	}
	// Exactly the five claims, each of its type.
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || len(fields) != 5 {
		return c, ErrClaims
	}
	for _, k := range []string{"n", "s", "p", "i", "e"} {
		if _, ok := fields[k]; !ok { // exactly these, in this case: Go's decoder alone would take "N"
			return c, ErrClaims
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&c) != nil {
		return Claims{}, ErrClaims
	}
	if !nodeID.MatchString(c.Node) || !sandboxID.MatchString(c.Sandbox) || !sessionID.MatchString(c.Session) || c.Port < 1 || c.Port > 65535 || c.Exp <= 0 {
		return Claims{}, ErrClaims
	}
	return c, nil
}

// Expired says whether the ticket's time is up at now (the Worker's clock, as
// the agent reckons it from welcome), allowing grace for the reckoning.
func (c Claims) Expired(now time.Time, grace time.Duration) bool {
	return !now.Before(time.Unix(c.Exp, 0).Add(grace))
}

func (c Claims) String() string {
	return fmt.Sprintf("%s %s:%d session %s", c.Node, c.Sandbox, c.Port, c.Session)
}
