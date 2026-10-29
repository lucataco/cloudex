// Package updatetest serves a fake GitHub release for tests of update.
package updatetest

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lucataco/cloudex/go/internal/update"
)

// Server serves release tag with exe as this platform's executable, and sets
// $CLOUDEX_RELEASES_URL to it. A non-empty badSum replaces the real checksum.
func Server(t testing.TB, tag string, exe []byte, badSum string) *httptest.Server {
	t.Helper()
	asset := update.Asset()
	archive := pack(t, asset, exe)
	sum := sha256.Sum256(archive)
	digest := hex.EncodeToString(sum[:])
	if badSum != "" {
		digest = badSum
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/releases/tag/"+tag, http.StatusFound)
	})
	mux.HandleFunc("/releases/download/"+tag+"/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%s  cloudex_other_os.tar.gz\n%s  %s\n", strings.Repeat("0", 64), digest, asset)
	})
	mux.HandleFunc("/releases/download/"+tag+"/"+asset, func(w http.ResponseWriter, r *http.Request) {
		w.Write(archive)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDEX_RELEASES_URL", srv.URL+"/releases")
	return srv
}

// pack builds a release archive the way scripts/build-cli.sh does.
func pack(t testing.TB, asset string, exe []byte) []byte {
	var buf bytes.Buffer
	files := []struct {
		name string
		data []byte
	}{{"LICENSE", []byte("MIT")}, {"cloudex", exe}, {"README.md", []byte("readme")}}
	if strings.HasSuffix(asset, ".zip") {
		zw := zip.NewWriter(&buf)
		for _, f := range files {
			name := f.name
			if name == "cloudex" {
				name = "cloudex.exe"
			}
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			w.Write(f.data)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range files {
		tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.data)), Typeflag: tar.TypeReg})
		tw.Write(f.data)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
