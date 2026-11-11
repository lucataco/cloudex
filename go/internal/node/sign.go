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
