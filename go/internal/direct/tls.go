package direct

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// The direct port's handshake, which the agent (go/internal/node, direct.go)
// serves and the CLI dials:
//
//  1. TLS 1.3 with ALPN "cloudex-direct/1". The certificate is self-signed
//     over the node's own Ed25519 key (NodeCert), the key the deployment
//     holds for the node; the client pins it (PinnedTLS), so no CA is
//     involved.
//  2. The client sends one line, "CXD1 <ticket>\n", at most LineMax bytes.
//  3. The agent answers "ok\n" and the connection carries the machine's port
//     from then on, or "err <reason>\n" and closes.
//
// Between the relay (the sidecar that Docker publishes the port on) and the
// agent, before all of that, the relay writes "CXR1 <client ip> <client
// port>\n" in the clear (RelayHeader).

// LineMax is the longest ticket line the agent reads, "\n" included.
const LineMax = 1024

// HandshakeTimeout is how long a client has from the TCP accept to its
// ticket line, the TLS handshake included.
const HandshakeTimeout = 5 * time.Second

// NodeCert is a self-signed certificate over the node's key, made in memory.
func NodeCert(key ed25519.PrivateKey) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 62))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "cloudex-node"},
		NotBefore:    now.Add(-24 * time.Hour),
		NotAfter:     now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// ServerTLS is the agent's TLS configuration: TLS 1.3 only, ALPN required
// (the caller checks NegotiatedProtocol, since Go accepts a client that
// offers no ALPN at all).
func ServerTLS(cert tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{ALPN},
	}
}

// ErrPin is a certificate that doesn't carry the pinned key.
var ErrPin = errors.New("the node's key doesn't match: not the node the deployment named")

// PinnedTLS is the client's TLS configuration: it accepts exactly the
// certificate carrying pub, whatever the address or name, and needs ALPN.
func PinnedTLS(pub ed25519.PublicKey) *tls.Config {
	pin := bytes.Clone(pub)
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		NextProtos:         []string{ALPN},
		InsecureSkipVerify: true, // the pin below replaces the chain and name checks
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) != 1 {
				return ErrPin
			}
			c, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return ErrPin
			}
			pk, ok := c.PublicKey.(ed25519.PublicKey)
			if !ok || len(pin) != ed25519.PublicKeySize || !bytes.Equal(pk, pin) {
				return ErrPin
			}
			// TLS 1.3's CertificateVerify, which crypto/tls checks against
			// this key, proves the peer holds its private half.
			return nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if cs.NegotiatedProtocol != ALPN {
				return fmt.Errorf("the server didn't agree to %s", ALPN)
			}
			return nil
		},
	}
}

// Refused is the agent's "err <reason>" answer to a ticket.
type Refused struct{ Reason string }

func (e *Refused) Error() string { return "the node refused the ticket: " + e.Reason }

// Handshake runs the client's side on an open TCP connection: TLS with the
// pin, then the ticket, then the agent's answer, read a byte at a time so
// that no byte of the stream after it is buffered away. The deadline bounds
// all of it. On success the returned connection carries the port.
func Handshake(ctx context.Context, raw net.Conn, pub ed25519.PublicKey, ticket string) (*tls.Conn, error) {
	c, err := HandshakeTLS(ctx, raw, pub)
	if err != nil {
		return nil, err
	}
	if err := PresentTicket(ctx, c, ticket); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// HandshakeTLS is the TLS part of Handshake alone: a client that races
// several candidates verifies each, and gives the ticket only to the winner.
func HandshakeTLS(ctx context.Context, raw net.Conn, pub ed25519.PublicKey) (*tls.Conn, error) {
	c := tls.Client(raw, PinnedTLS(pub))
	if err := c.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
}

// PresentTicket sends the ticket line and reads the answer.
func PresentTicket(ctx context.Context, c net.Conn, ticket string) error {
	if strings.ContainsAny(ticket, " \r\n") || len(ticket) > TicketMax {
		return errors.New("not a ticket")
	}
	if d, ok := ctx.Deadline(); ok {
		c.SetDeadline(d)
		defer c.SetDeadline(time.Time{})
	}
	if _, err := io.WriteString(c, "CXD1 "+ticket+"\n"); err != nil {
		return err
	}
	line, err := ReadLine(c, 256)
	if err != nil {
		return fmt.Errorf("reading the node's answer: %w", err)
	}
	if line == "ok" {
		return nil
	}
	if r, ok := strings.CutPrefix(line, "err "); ok {
		return &Refused{Reason: r}
	}
	return fmt.Errorf("the node answered %q", line)
}

// ErrLineTooLong is a line over its limit.
var ErrLineTooLong = errors.New("line too long")

// ReadLine reads up to "\n", one byte at a time, at most max bytes with the
// "\n", and returns the line without it.
func ReadLine(r io.Reader, max int) (string, error) {
	buf := make([]byte, 0, 64)
	var b [1]byte
	for {
		n, err := r.Read(b[:])
		if n == 1 {
			if b[0] == '\n' {
				return string(buf), nil
			}
			if len(buf)+1 >= max {
				return "", ErrLineTooLong
			}
			buf = append(buf, b[0])
			continue
		}
		if err != nil {
			if err == io.EOF && len(buf) > 0 {
				err = io.ErrUnexpectedEOF
			}
			return "", err
		}
	}
}

// RelayHeaderMax is the longest relay header, "\n" included.
const RelayHeaderMax = 64

// RelayHeader is the line the relay writes to the agent first.
func RelayHeader(client netip.AddrPort) string {
	return fmt.Sprintf("CXR1 %s %d\n", client.Addr().Unmap(), client.Port())
}

// ParseRelayHeader reads RelayHeader's line (without its "\n"): an IPv4
// client address and its port.
func ParseRelayHeader(line string) (netip.AddrPort, error) {
	f := strings.Split(line, " ")
	if len(f) != 3 || f[0] != "CXR1" {
		return netip.AddrPort{}, errors.New("not a relay header")
	}
	a, err := netip.ParseAddr(f[1])
	if err != nil || !a.Is4() {
		return netip.AddrPort{}, errors.New("the relay header's address isn't IPv4")
	}
	var p uint16
	if _, err := fmt.Sscanf(f[2], "%d", &p); err != nil || fmt.Sprint(p) != f[2] || p == 0 {
		return netip.AddrPort{}, errors.New("the relay header's port isn't one")
	}
	return netip.AddrPortFrom(a, p), nil
}

var (
	rfc1918 = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.168.0.0/16")}
	shared  = netip.MustParsePrefix("100.64.0.0/10")
	tsULA   = netip.MustParsePrefix("fd7a:115c:a1e0::/48")
)

// Private says whether a is an RFC 1918 IPv4 address.
func Private(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range rfc1918 {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// IsTailscale says whether a is in 100.64.0.0/10 (shared address space,
// which Tailscale uses) or Tailscale's IPv6 range fd7a:115c:a1e0::/48.
func IsTailscale(a netip.Addr) bool {
	a = a.Unmap()
	return shared.Contains(a) || tsULA.Contains(a)
}

// Usable says whether a direct port may be published on a, or a client may
// come from it: an IPv4 address that is RFC 1918 or shared (100.64/10).
func Usable(a netip.Addr) bool {
	a = a.Unmap()
	return a.Is4() && (Private(a) || shared.Contains(a))
}

// SpliceBuf is the copy buffer of each direction of a splice.
const SpliceBuf = 256 << 10

var bufs = sync.Pool{New: func() any { b := make([]byte, SpliceBuf); return &b }}

// Splice copies both ways until both directions end. An EOF from one side
// is passed on as a half-close (CloseWrite) where the other side has one,
// since the port may carry any TCP protocol; an error ends both. fromClient,
// when set, is called with the size of each read from client. It returns
// when both directions are done, with both connections closed.
func Splice(client, server net.Conn, fromClient func(int)) {
	done := make(chan struct{}, 2)
	var once sync.Once
	closeBoth := func() { client.Close(); server.Close() }
	cp := func(dst, src net.Conn, count func(int)) {
		bp := bufs.Get().(*[]byte)
		defer bufs.Put(bp)
		buf := *bp
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if count != nil {
					count(n)
				}
				if _, werr := dst.Write(buf[:n]); werr != nil {
					once.Do(closeBoth)
					break
				}
			}
			if err != nil {
				if err == io.EOF {
					if cw, ok := dst.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
						break
					}
				}
				once.Do(closeBoth)
				break
			}
		}
		done <- struct{}{}
	}
	go cp(server, client, fromClient)
	go cp(client, server, nil)
	<-done
	<-done
	once.Do(closeBoth)
}
