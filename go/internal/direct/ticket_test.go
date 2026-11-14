package direct

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// The vectors apps/api/test/direct.test.ts checks the Worker against too.
type vectors struct {
	Label     string `json:"label"`
	AdminKey  string `json:"adminKey"`
	Node      string `json:"node"`
	PublicKey string `json:"publicKey"`
	Secret    string `json:"secret"`
	NowMs     int64  `json:"nowMs"`
	TTLSec    int    `json:"ttlSec"`
	Max       int    `json:"max"`
	Valid     []struct {
		Name   string `json:"name"`
		Ticket string `json:"ticket"`
		Claims Claims `json:"claims"`
	} `json:"valid"`
	Invalid []struct {
		Name   string `json:"name"`
		Ticket string `json:"ticket"`
	} `json:"invalid"`
}

func load(t *testing.T) vectors {
	t.Helper()
	b, err := os.ReadFile("../../../packages/schema/fixtures/node/protocol.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Direct vectors `json:"direct"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if len(f.Direct.Valid) == 0 || len(f.Direct.Invalid) < 10 {
		t.Fatal("no direct vectors in the fixture")
	}
	return f.Direct
}

func TestTheConstantsMatchTheFixture(t *testing.T) {
	v := load(t)
	if v.Label != Label || time.Duration(v.TTLSec)*time.Second != TicketTTL || v.Max != TicketMax {
		t.Fatalf("fixture %q %d %d, Go %q %v %d", v.Label, v.TTLSec, v.Max, Label, TicketTTL, TicketMax)
	}
}

func TestTheSecretIsDerivedAsTheWorkerDerivesIt(t *testing.T) {
	v := load(t)
	got := base64.StdEncoding.EncodeToString(Secret(v.AdminKey, v.Node, v.PublicKey))
	if got != v.Secret {
		t.Fatalf("secret %s, fixture %s", got, v.Secret)
	}
	if _, err := DecodeSecret(v.Secret); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "c2hvcnQ=", "not base64!", strings.Repeat("A", 44)} {
		if _, err := DecodeSecret(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	// Another node, key or admin key: another secret.
	for _, s := range [][]byte{Secret(v.AdminKey, "nd-zzzz9999", v.PublicKey), Secret(v.AdminKey, v.Node, "x"), Secret("other", v.Node, v.PublicKey)} {
		if base64.StdEncoding.EncodeToString(s) == v.Secret {
			t.Fatal("the secret doesn't depend on all three")
		}
	}
}

func TestValidTicketsVerifyAndMintTheSame(t *testing.T) {
	v := load(t)
	secret, _ := DecodeSecret(v.Secret)
	now := time.UnixMilli(v.NowMs)
	for _, e := range v.Valid {
		c, err := Verify(secret, e.Ticket)
		if err != nil {
			t.Fatalf("%s: %v", e.Name, err)
		}
		if c != e.Claims {
			t.Fatalf("%s: %+v, fixture %+v", e.Name, c, e.Claims)
		}
		if Mint(secret, c) != e.Ticket {
			t.Fatalf("%s: Go mints %s", e.Name, Mint(secret, c))
		}
		if c.Expired(now, 0) || c.Expired(now.Add(TicketTTL-time.Second), 0) {
			t.Fatalf("%s: expired too soon", e.Name)
		}
		if !c.Expired(now.Add(TicketTTL), 0) || c.Expired(now.Add(TicketTTL), 10*time.Second) || !c.Expired(now.Add(TicketTTL+10*time.Second), 10*time.Second) {
			t.Fatalf("%s: the expiry or its grace is off", e.Name)
		}
	}
}

func TestInvalidTicketsAreRefused(t *testing.T) {
	v := load(t)
	secret, _ := DecodeSecret(v.Secret)
	for _, e := range v.Invalid {
		if c, err := Verify(secret, e.Ticket); err == nil {
			t.Errorf("%s: accepted as %+v", e.Name, c)
		}
	}
	// A valid ticket under another secret.
	other := Secret(v.AdminKey, "nd-zzzz9999", v.PublicKey)
	if _, err := Verify(other, v.Valid[0].Ticket); err != ErrBadMAC {
		t.Fatalf("another secret: %v", err)
	}
	if _, err := Verify(secret, strings.Repeat("a", TicketMax+1)); err != ErrTooLong {
		t.Fatalf("too long: %v", err)
	}
}

func TestNewSessionsAreHexAndDiffer(t *testing.T) {
	a, b := NewSession(), NewSession()
	if !sessionID.MatchString(a) || a == b {
		t.Fatalf("%s %s", a, b)
	}
}
