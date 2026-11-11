package node

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// The node CA (v0.2.2 M6, decision 3). A machine's HTTPS calls to the AI
// providers whose keys the deployment holds go to the agent (KEYS_SCRIPT in
// apps/api/src/keys.ts points those hosts at it), which ends their TLS with a
// certificate from this CA and sends the request on to the deployment, where
// the real key is added (egress.go). The CA:
//
//   - is made here, on the node; its private key (ca.key, 0600) never leaves
//     the agent's state directory, and only its certificate is handed out;
//   - is name-constrained (critical) to exactly the provider hosts the
//     deployment names in its welcome message: each is permitted, and its
//     subdomains are excluded, so a certificate it signs for any other name
//     (the deployment's own API, github.com, pypi.org) fails verification in
//     every client that checks constraints, which OpenSSL, BoringSSL, Go and
//     rustls all do;
//   - is made again when that list changes, so a provider added later is
//     covered once machines restart (they trust the CA at each start).
//
// Leaf certificates are made per host on first use and kept in memory.

// CAHosts are the hosts a CA may sign for: lower-case DNS names, sorted.
var caHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// MaxCAHosts bounds the welcome's list.
const MaxCAHosts = 32

// CleanHosts checks and sorts a list of provider hosts.
func CleanHosts(hosts []string) ([]string, error) {
	if len(hosts) > MaxCAHosts {
		return nil, fmt.Errorf("%d provider hosts; at most %d", len(hosts), MaxCAHosts)
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		h = strings.ToLower(h)
		if len(h) > 253 || !caHostPattern.MatchString(h) {
			return nil, fmt.Errorf("%q isn't a host name", h)
		}
		if !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out, nil
}

// CA is the node's CA and the leaf certificates it has made.
type CA struct {
	Hosts []string
	Cert  *x509.Certificate
	PEM   []byte // the certificate, which machines trust
	key   *ecdsa.PrivateKey

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
	now    func() time.Time
}

type caMeta struct {
	Hosts []string `json:"hosts"`
}

// CA lifetimes. A leaf is made again a day before it ends.
const (
	caLifetime   = 5 * 365 * 24 * time.Hour
	leafLifetime = 30 * 24 * time.Hour
)

// NewCA makes a CA for hosts.
func NewCA(nodeID string, hosts []string, now time.Time) (*CA, error) {
	hosts, err := CleanHosts(hosts)
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, errors.New("no provider hosts to make a CA for")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	excluded := make([]string, len(hosts))
	for i, h := range hosts {
		excluded[i] = "." + h
	}
	tmpl := &x509.Certificate{
		SerialNumber:                serial(),
		Subject:                     pkix.Name{CommonName: "Cloudex node CA " + nodeID, Organization: []string{"Cloudex"}},
		NotBefore:                   now.Add(-time.Hour),
		NotAfter:                    now.Add(caLifetime),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         hosts,
		ExcludedDNSDomains:          excluded,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Hosts: hosts, Cert: cert, PEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), key: key}, nil
}

func serial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	return n.Add(n, big.NewInt(1))
}

// Fingerprint is the SHA-256 of the CA's certificate, in hex.
func (c *CA) Fingerprint() string {
	sum := sha256.Sum256(c.Cert.Raw)
	return hex.EncodeToString(sum[:])
}

// Allows says whether the CA serves host: exactly one of its hosts.
func (c *CA) Allows(host string) bool {
	return slices.Contains(c.Hosts, strings.ToLower(host))
}

// Leaf is a certificate for host, made on first use. Hosts outside the list
// are refused here, before anything is signed.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	host = strings.ToLower(host)
	if !c.Allows(host) {
		return nil, fmt.Errorf("the node CA doesn't serve %q", host)
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if l := c.leaves[host]; l != nil && l.Leaf.NotAfter.After(now.Add(24*time.Hour)) {
		return l, nil
	}
	l, err := c.sign(host, now)
	if err != nil {
		return nil, err
	}
	if c.leaves == nil {
		c.leaves = map[string]*tls.Certificate{}
	}
	c.leaves[host] = l
	return l, nil
}

// sign makes a leaf for any name. Only Leaf (for the CA's own hosts) and
// tests call it.
func (c *CA) sign(host string, now time.Time) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: host},
		DNSNames:              []string{host},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(leafLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.Cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, nil
}

// SignAnyForTest signs a certificate for any name with the CA's key, as a
// stolen key could: the release checks hand it to a machine to show that it
// fails verification there for any name the CA doesn't cover (cloudex-node
// ca test-cert). It needs the CA's key, so it runs only where that is.
// The output is the certificate, then its own key (which a test server needs).
func (c *CA) SignAnyForTest(host string) (certAndKeyPEM []byte, err error) {
	l, err := c.sign(host, time.Now())
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(l.PrivateKey.(*ecdsa.PrivateKey))
	if err != nil {
		return nil, err
	}
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: l.Certificate[0]})
	return append(out, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})...), nil
}

// LoadCA reads the CA from the state directory; nil when there's none.
func (s State) LoadCA() (*CA, error) {
	certPEM, err := os.ReadFile(s.path("ca.crt"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(s.path("ca.key"))
	if err != nil {
		return nil, err
	}
	var meta caMeta
	if b, err := os.ReadFile(s.path("ca.json")); err == nil {
		json.Unmarshal(b, &meta)
	}
	cb, _ := pem.Decode(certPEM)
	kb, _ := pem.Decode(keyPEM)
	if cb == nil || kb == nil {
		return nil, errors.New("the node CA's files are damaged")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	hosts, err := CleanHosts(meta.Hosts)
	if err != nil || !slices.Equal(hosts, sortedCopy(cert.PermittedDNSDomains)) {
		return nil, errors.New("the node CA's host list doesn't match its certificate")
	}
	return &CA{Hosts: hosts, Cert: cert, PEM: certPEM, key: key}, nil
}

func sortedCopy(v []string) []string {
	out := append([]string(nil), v...)
	sort.Strings(out)
	return out
}

// SaveCA writes the CA: its key 0600, never anywhere else.
func (s State) SaveCA(c *CA) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	der, err := x509.MarshalECPrivateKey(c.key)
	if err != nil {
		return err
	}
	if err := writeAtomic(s.path("ca.key"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return err
	}
	if err := writeAtomic(s.path("ca.crt"), c.PEM, 0o644); err != nil {
		return err
	}
	return s.write("ca.json", caMeta{Hosts: c.Hosts}, 0o644)
}

func writeAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// EnsureCA is the CA for hosts: the one kept in the state directory when it
// covers exactly those hosts, otherwise a new one, which replaces it.
func (s State) EnsureCA(nodeID string, hosts []string) (*CA, bool, error) {
	want, err := CleanHosts(hosts)
	if err != nil {
		return nil, false, err
	}
	if have, err := s.LoadCA(); err == nil && have != nil && slices.Equal(have.Hosts, want) && have.Cert.NotAfter.After(time.Now().Add(30*24*time.Hour)) {
		return have, false, nil
	}
	c, err := NewCA(nodeID, want, time.Now())
	if err != nil {
		return nil, false, err
	}
	if s.Dir != "" {
		if err := s.SaveCA(c); err != nil {
			return nil, false, err
		}
	}
	return c, true, nil
}
