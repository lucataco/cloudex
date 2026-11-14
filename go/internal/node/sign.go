package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"
)

// Headers of a signed request (apps/api/src/nodeauth.ts).
const (
	HeaderNode     = "x-cx-node"
	HeaderTS       = "x-cx-ts"
	HeaderNonce    = "x-cx-nonce"
	HeaderSig      = "x-cx-sig"
	HeaderSandbox  = "x-cx-sandbox"
	HeaderProtocol = "x-cx-protocol"
)

// SigningString is what a node signs: METHOD\nPATH?QUERY\nNODE\nSANDBOX\nTS\nNONCE.
// NODE is the node's id, empty for a join (2026-11-12 review, W6).
//
// The node key signs three things (2026-11-14 review, D-note; plan D13):
// these strings; its own TLS certificate for the direct port (x509, whose
// signed part is DER and starts with 0x30); and, in each TLS 1.3 handshake
// on that port, a CertificateVerify, whose signed content starts with 64
// bytes of 0x20 (RFC 8446 4.4.3). A signature over one can't serve as
// another only because their first bytes differ: here, METHOD is an HTTP
// method token (uppercase letters), which Sign enforces itself rather than
// trusting http.NewRequest to have refused one with a space. A context
// prefix in the string would say it outright, but v0.2.2 Workers check this
// exact string, so the argument is kept in this comment and the check.
func SigningString(method, pathQuery, node, sandbox, ts, nonce string) string {
	return method + "\n" + pathQuery + "\n" + node + "\n" + sandbox + "\n" + ts + "\n" + nonce
}

// Signer signs requests with a node's key. Now and Nonce are replaced in tests.
type Signer struct {
	Key   ed25519.PrivateKey
	Node  string // the node's id; empty for a join, which the Worker checks against the key it brings
	Now   func() time.Time
	Nonce func() string
}

func randomNonce() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Sign sets the signature headers on h for a request to pathQuery (the path
// with its query, as sent).
func (s Signer) Sign(h http.Header, method, pathQuery, sandbox string) {
	now, nonce := time.Now, randomNonce
	if s.Now != nil {
		now = s.Now
	}
	if s.Nonce != nil {
		nonce = s.Nonce
	}
	// Never sign a string that could start like another protocol's signed
	// content (see SigningString): the request goes unsigned, and is refused.
	if !methodToken(method) {
		return
	}
	ts := strconv.FormatInt(now().Unix(), 10)
	n := nonce()
	sig := ed25519.Sign(s.Key, []byte(SigningString(method, pathQuery, s.Node, sandbox, ts, n)))
	if s.Node != "" {
		h.Set(HeaderNode, s.Node)
	}
	if sandbox != "" {
		h.Set(HeaderSandbox, sandbox)
	}
	h.Set(HeaderTS, ts)
	h.Set(HeaderNonce, n)
	h.Set(HeaderSig, base64.StdEncoding.EncodeToString(sig))
}

// PublicKey is the key in the form the Worker stores: raw, in base64.
func PublicKey(k ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(k.Public().(ed25519.PublicKey))
}

// methodToken says whether m is an HTTP method as the agent sends them:
// 1 to 16 uppercase ASCII letters.
func methodToken(m string) bool {
	if len(m) == 0 || len(m) > 16 {
		return false
	}
	for i := 0; i < len(m); i++ {
		if m[i] < 'A' || m[i] > 'Z' {
			return false
		}
	}
	return true
}
