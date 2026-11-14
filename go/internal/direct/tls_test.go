package direct

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// serveOnce runs a TLS server with cfg on a local port and returns its
// address; the server answers "ok\n" to any line after the handshake.
func serveOnce(t *testing.T, cfg *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				tc := tls.Server(c, cfg)
				if tc.Handshake() != nil {
					return
				}
				if _, err := ReadLine(tc, LineMax); err == nil {
					io.WriteString(tc, "ok\n")
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func dial(t *testing.T, addr string, pub ed25519.PublicKey) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Handshake(ctx, raw, pub, "a.b")
	if err == nil {
		c.Close()
	}
	return err
}

func TestPinnedTLS(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	cert, err := NodeCert(key)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveOnce(t, ServerTLS(cert))
	if err := dial(t, addr, pub); err != nil {
		t.Fatalf("the pinned key: %v", err)
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if err := dial(t, addr, other); err == nil || !strings.Contains(err.Error(), ErrPin.Error()) {
		t.Fatalf("another key pinned: %v", err)
	}
	if err := dial(t, addr, nil); err == nil {
		t.Fatal("no key pinned: connected")
	}
}

func TestPinnedTLSRefusesECDSA(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(nil)
	ek, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &ek.PublicKey, ek)
	cfg := ServerTLS(tls.Certificate{Certificate: [][]byte{der}, PrivateKey: ek})
	addr := serveOnce(t, cfg)
	if err := dial(t, addr, pub); err == nil {
		t.Fatal("an ECDSA certificate passed the pin")
	}
}

func TestALPNRequired(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(nil)
	cert, _ := NodeCert(key)
	cfg := ServerTLS(cert)
	cfg.NextProtos = []string{"h2"}
	addr := serveOnce(t, cfg)
	if err := dial(t, addr, pub); err == nil {
		t.Fatal("a server without cloudex-direct/1 passed")
	}
	// A server that offers no ALPN at all: the client refuses it too.
	cfg2 := ServerTLS(cert)
	cfg2.NextProtos = nil
	addr2 := serveOnce(t, cfg2)
	if err := dial(t, addr2, pub); err == nil {
		t.Fatal("a server with no ALPN passed")
	}
}

func TestPresentTicketRefused(t *testing.T) {
	a, b := net.Pipe()
	go func() {
		ReadLine(b, LineMax)
		io.WriteString(b, "err used already\n")
		b.Close()
	}()
	err := PresentTicket(context.Background(), a, "x.y")
	var r *Refused
	if !errors.As(err, &r) || r.Reason != "used already" {
		t.Fatalf("got %v", err)
	}
	if PresentTicket(context.Background(), a, "x y") == nil {
		t.Fatal("a ticket with a space was sent")
	}
}

func TestReadLine(t *testing.T) {
	for _, c := range []struct {
		in, want string
		max      int
		err      bool
	}{
		{"CXD1 abc\nrest", "CXD1 abc", 64, false},
		{"\n", "", 64, false},
		{strings.Repeat("a", 63) + "\n", strings.Repeat("a", 63), 64, false},
		{strings.Repeat("a", 64) + "\n", "", 64, true},
		{"no newline", "", 64, true},
	} {
		r := strings.NewReader(c.in)
		got, err := ReadLine(r, c.max)
		if (err != nil) != c.err || got != c.want {
			t.Errorf("%q: got %q, %v", c.in, got, err)
		}
		if c.in == "CXD1 abc\nrest" {
			rest, _ := io.ReadAll(r)
			if string(rest) != "rest" {
				t.Errorf("read past the line: %q left", rest)
			}
		}
	}
}

func TestRelayHeader(t *testing.T) {
	ap := netip.MustParseAddrPort("192.168.65.1:51234")
	h := RelayHeader(ap)
	if h != "CXR1 192.168.65.1 51234\n" {
		t.Fatalf("header %q", h)
	}
	got, err := ParseRelayHeader(strings.TrimSuffix(h, "\n"))
	if err != nil || got != ap {
		t.Fatalf("parsed %v %v", got, err)
	}
	mapped := netip.AddrPortFrom(netip.MustParseAddr("::ffff:10.0.0.5"), 9)
	if RelayHeader(mapped) != "CXR1 10.0.0.5 9\n" {
		t.Fatalf("a mapped address: %q", RelayHeader(mapped))
	}
	for _, bad := range []string{"", "CXR1", "CXR1 1.2.3.4", "CXR2 1.2.3.4 5", "CXR1 1.2.3.4 0", "CXR1 1.2.3.4 65536", "CXR1 1.2.3.4 +5",
		"CXR1 1.2.3.4 05", "CXR1 ::1 5", "CXR1 1.2.3.4 5 6", "CXR1  1.2.3.4 5", "CXR1 host 5", "cxr1 1.2.3.4 5"} {
		if _, err := ParseRelayHeader(bad); err == nil {
			t.Errorf("took %q", bad)
		}
	}
}

func TestUsable(t *testing.T) {
	for a, want := range map[string]bool{
		"10.1.2.3": true, "172.16.0.1": true, "172.31.255.255": true, "192.168.65.2": true, "100.64.0.1": true, "100.124.24.58": true,
		"172.32.0.1": false, "100.128.0.1": false, "8.8.8.8": false, "0.0.0.0": false, "127.0.0.1": false, "169.254.1.1": false,
		"::": false, "fd7a:115c:a1e0::1": false, "::ffff:192.168.1.1": true,
	} {
		if got := Usable(netip.MustParseAddr(a)); got != want {
			t.Errorf("Usable(%s) = %v", a, got)
		}
	}
	if !IsTailscale(netip.MustParseAddr("fd7a:115c:a1e0::5")) || IsTailscale(netip.MustParseAddr("fd7a:115c:a1e1::5")) {
		t.Error("IsTailscale's v6 range")
	}
}

// halfConn is a TCP pair, so CloseWrite works.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn)
	go func() { c, _ := ln.Accept(); ch <- c }()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return a, <-ch
}

func TestSpliceHalfClose(t *testing.T) {
	client, clientEnd := tcpPair(t)
	serverEnd, server := tcpPair(t)
	var got int
	done := make(chan struct{})
	go func() { Splice(clientEnd, serverEnd, func(n int) { got += n }); close(done) }()
	// The client sends and half-closes; the server still answers after
	// reading the EOF (a protocol that needs half-close).
	io.WriteString(client, "hello")
	client.(*net.TCPConn).CloseWrite()
	b, err := io.ReadAll(server)
	if err != nil || string(b) != "hello" {
		t.Fatalf("server read %q %v", b, err)
	}
	io.WriteString(server, "answer")
	server.Close()
	b, err = io.ReadAll(client)
	if err != nil || string(b) != "answer" {
		t.Fatalf("client read %q %v", b, err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the splice didn't end")
	}
	if got != 5 {
		t.Fatalf("counted %d client bytes", got)
	}
}
