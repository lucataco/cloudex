package node

import (
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"
)

var testHosts = []string{"api.anthropic.com", "api.openai.com", "openrouter.ai", "generativelanguage.googleapis.com"}

func verifyWith(t *testing.T, ca *CA, certPEM []byte, name string) error {
	t.Helper()
	b, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.PEM)
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return err
}

func TestCAServesOnlyItsHosts(t *testing.T) {
	ca, err := NewCA("nd-test0001", testHosts, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Cert.IsCA || !ca.Cert.MaxPathLenZero || !ca.Cert.PermittedDNSDomainsCritical {
		t.Fatalf("CA flags: isCA %v, maxPathLenZero %v, critical %v", ca.Cert.IsCA, ca.Cert.MaxPathLenZero, ca.Cert.PermittedDNSDomainsCritical)
	}
	for _, h := range testHosts {
		l, err := ca.Leaf(h)
		if err != nil {
			t.Fatalf("leaf for %s: %v", h, err)
		}
		p := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: l.Certificate[0]})
		if err := verifyWith(t, ca, p, h); err != nil {
			t.Fatalf("a leaf for %s doesn't verify: %v", h, err)
		}
	}
	// The agent never signs another name...
	for _, h := range []string{"github.com", "pypi.org", "cloudex-api.catacolabs.workers.dev", "x.api.anthropic.com", "example.com"} {
		if _, err := ca.Leaf(h); err == nil {
			t.Fatalf("Leaf signed %s", h)
		}
	}
	// ...and one signed with its key anyway (a stolen key) fails verification:
	// the name constraints hold, including for subdomains of a provider host.
	for _, h := range []string{"github.com", "pypi.org", "cloudex-api.catacolabs.workers.dev", "x.api.anthropic.com", "anthropic.com", "example.com", "registry.npmjs.org"} {
		p, err := ca.SignAnyForTest(h)
		if err != nil {
			t.Fatal(err)
		}
		err = verifyWith(t, ca, p, h)
		if err == nil || !strings.Contains(err.Error(), "not") {
			t.Fatalf("a certificate for %s signed by the node CA verified (err %v)", h, err)
		}
	}
}

func TestCleanHosts(t *testing.T) {
	got, err := CleanHosts([]string{"API.OpenAI.com", "api.anthropic.com", "api.openai.com"})
	if err != nil || strings.Join(got, ",") != "api.anthropic.com,api.openai.com" {
		t.Fatalf("got %v %v", got, err)
	}
	for _, bad := range []string{"", "*.openai.com", "localhost", "a..b", "-a.com", "a.com/x", "1.2.3.4:443"} {
		if _, err := CleanHosts([]string{bad}); err == nil && bad != "1.2.3.4:443" {
			t.Fatalf("%q was accepted", bad)
		}
	}
	if _, err := CleanHosts(make([]string, MaxCAHosts+1)); err == nil {
		t.Fatal("too many hosts were accepted")
	}
}

func TestEnsureCAKeepsAndReplaces(t *testing.T) {
	s := State{Dir: t.TempDir()}
	a, made, err := s.EnsureCA("nd-test0001", testHosts)
	if err != nil || !made {
		t.Fatalf("first: %v %v", made, err)
	}
	fi, err := os.Stat(s.path("ca.key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("ca.key mode %v %v", fi.Mode().Perm(), err)
	}
	b, made, err := s.EnsureCA("nd-test0001", []string{"api.openai.com", "api.anthropic.com", "openrouter.ai", "generativelanguage.googleapis.com"})
	if err != nil || made || b.Fingerprint() != a.Fingerprint() {
		t.Fatalf("the same hosts made a new CA: %v %v", made, err)
	}
	c, made, err := s.EnsureCA("nd-test0001", append(testHosts, "api.x.ai"))
	if err != nil || !made || c.Fingerprint() == a.Fingerprint() || !c.Allows("api.x.ai") {
		t.Fatalf("a new host kept the old CA: %v %v", made, err)
	}
	// What's on disk is the new one, and it loads.
	d, err := s.LoadCA()
	if err != nil || d.Fingerprint() != c.Fingerprint() {
		t.Fatalf("load: %v", err)
	}
}
