// Package update finds, checks and installs CLI releases from GitHub, the
// same files the install scripts use: cloudex_<os>_<arch>.tar.gz (.zip on
// Windows) and checksums.txt.
package update

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// ReleasesURL is where releases live; $CLOUDEX_RELEASES_URL replaces it.
const ReleasesURL = "https://github.com/lucataco/cloudex/releases"

func releasesURL() string {
	if u := os.Getenv("CLOUDEX_RELEASES_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return ReleasesURL
}

var client = &http.Client{
	Timeout: 5 * time.Minute,
	// Latest reads the redirect itself.
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if strings.HasSuffix(via[0].URL.Path, "/latest") {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

// Latest returns the newest release's tag, read from where /releases/latest
// redirects (which, unlike the GitHub API, has no rate limit).
func Latest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL()+"/latest", nil)
	if err != nil {
		return "", err
	}
	res, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("checking for a new version: %w", err)
	}
	res.Body.Close()
	tag := path.Base(res.Header.Get("Location"))
	if res.StatusCode/100 != 3 || !strings.HasPrefix(tag, "v") {
		return "", fmt.Errorf("checking for a new version: %s answered %s", releasesURL(), res.Status)
	}
	return tag, nil
}

// Newer reports whether version a is newer than b. Versions look like v1.2.3;
// anything after a dash (v1.2.3-dev) sorts before the plain version.
func Newer(a, b string) bool {
	pa, preA := parse(a)
	pb, preB := parse(b)
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return preB && !preA
}

func parse(v string) (nums [3]int, pre bool) {
	v = strings.TrimPrefix(v, "v")
	if i := strings.IndexByte(v, '-'); i >= 0 {
		v, pre = v[:i], true
	}
	for i, part := range strings.SplitN(v, ".", 3) {
		nums[i], _ = strconv.Atoi(part)
	}
	return nums, pre
}

// Asset is this platform's archive name.
func Asset() string {
	if runtime.GOOS == "windows" {
		return fmt.Sprintf("cloudex_%s_%s.zip", runtime.GOOS, runtime.GOARCH)
	}
	return fmt.Sprintf("cloudex_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
}

// Download fetches tag's archive for this platform, checks it against the
// release's checksums.txt and returns the cloudex executable inside.
func Download(ctx context.Context, tag string) ([]byte, error) {
	base := fmt.Sprintf("%s/download/%s/", releasesURL(), tag)
	sums, err := get(ctx, base+"checksums.txt")
	if err != nil {
		return nil, err
	}
	asset := Asset()
	want := ""
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for scanner.Scan() {
		if f := strings.Fields(scanner.Text()); len(f) == 2 && f[1] == asset {
			want = f[0]
		}
	}
	if want == "" {
		return nil, fmt.Errorf("%s has no build for this computer (%s)", tag, asset)
	}
	archive, err := get(ctx, base+asset)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(archive)
	if hex.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("%s doesn't match its checksum; not installing it", asset)
	}
	if strings.HasSuffix(asset, ".zip") {
		return fromZip(archive)
	}
	return fromTarGz(archive)
}

func get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, res.Status)
	}
	return io.ReadAll(res.Body)
}

func fromTarGz(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errors.New("the archive has no cloudex executable")
			}
			return nil, err
		}
		if path.Base(h.Name) == "cloudex" && h.Typeflag == tar.TypeReg {
			return io.ReadAll(tr)
		}
	}
}

func fromZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, f := range zr.File {
		if path.Base(f.Name) == "cloudex.exe" {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, errors.New("the archive has no cloudex.exe")
}

// Replace puts exe in place of the executable at target. The new file is
// written next to it and renamed over it, so a failure leaves the old one.
// Windows won't overwrite a running executable but will rename it, so there
// the old one moves to <target>.old, which the next update deletes.
func Replace(target string, exe []byte) error {
	dir := filepath.Dir(target)
	tmp, err := os.CreateTemp(dir, ".cloudex-update-*")
	if err != nil {
		return writeError(target, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(exe); err != nil {
		tmp.Close()
		return writeError(target, err)
	}
	if err := tmp.Close(); err != nil {
		return writeError(target, err)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return writeError(target, err)
	}
	if runtime.GOOS == "windows" {
		old := target + ".old"
		os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			return writeError(target, err)
		}
		if err := os.Rename(tmp.Name(), target); err != nil {
			os.Rename(old, target)
			return writeError(target, err)
		}
		return nil
	}
	if err := os.Rename(tmp.Name(), target); err != nil {
		return writeError(target, err)
	}
	return nil
}

// CleanUp deletes what an earlier update on Windows left behind.
func CleanUp(target string) {
	os.Remove(target + ".old")
}

func writeError(target string, err error) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("can't replace %s: permission denied. Run the update with sudo, or reinstall with the install script", target)
	}
	return fmt.Errorf("replacing %s: %w", target, err)
}
