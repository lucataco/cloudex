// Package deploy installs or updates Cloudex in a Cloudflare account.
//
// The steps, in order:
//  1. Check the token and that the account can run containers (Workers Paid).
//  2. Copy the published sandbox image into the account's own registry. The
//     durable_object scheduling policy only runs digest-pinned images from
//     that registry, and this needs no Docker on the user's machine.
//  3. Find or create the D1 database, and the preview zone's wildcard record.
//  4. Upload the embedded API Worker with wrangler, which also creates the
//     container application, and set the admin key as a secret.
//  5. Wait for the API, then start, use and delete a throwaway sandbox,
//     because a newly pushed image can take a while to reach every host.
package deploy

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/cloudflare"
)

//go:embed assets
var assets embed.FS

// DefaultImage is the published sandbox image. Release builds pin it to their
// version with -ldflags "-X github.com/lucataco/cloudex/go/internal/deploy.DefaultImage=...".
var DefaultImage = "ghcr.io/lucataco/cloudex-base:edge"

const (
	WorkerName      = "cloudex-api"
	BucketName      = "cloudex-backups"
	DatabaseName    = "cloudex"
	imageRepo       = "cloudex-base"
	registryHost    = "registry.cloudflare.com"
	wranglerVersion = "4.154.0"
)

// TokenURL opens the dashboard with an account API token pre-filled with what
// deploy needs. Routes, DNS and Zone are only used for preview links, and R2
// only for backups; deploy carries on without them.
// The dashboard rejects the whole link if an entry has no type, even though it
// ignores the type when the key is a permission group ID.
var TokenURL = tokenURL([]tokenPermission{
	{"e086da7e2179491d91ee5f35b3ca210a", "edit"}, // Workers Scripts Write
	{"bdbcd690c763475a985e8641dddc09f7", "edit"}, // Workers Containers Write
	{"09b2857d1c31407795e75e3fed8617a1", "edit"}, // D1 Write
	{"c1fde68c7bcc44588cbb6ddbc16d6480", "read"}, // Account Settings Read
	{"28f4b596e7d643029c524985477ae49a", "edit"}, // Workers Routes Write
	{"4755a26eedb94da69e1066d98aa820be", "edit"}, // DNS Write
	{"c8fed203ed3043cba015a93ad1616f1f", "read"}, // Zone Read
	{"workers_r2", "edit"},                       // Workers R2 Storage Write (the documented short key)
})

type tokenPermission struct {
	Key  string `json:"key"`
	Type string `json:"type"`
}

func tokenURL(perms []tokenPermission) string {
	keys, _ := json.Marshal(perms)
	// The documented shape: https://developers.cloudflare.com/fundamentals/api/how-to/account-owned-token-template/
	return "https://dash.cloudflare.com/?to=/:account/api-tokens&permissionGroupKeys=" + url.QueryEscape(string(keys)) + "&name=Cloudex"
}

type Options struct {
	Token     string
	AccountID string
	// PreviewDomain is used when PreviewDomainSet (an empty one turns previews
	// off). Otherwise deploy keeps the live deployment's domain, or failing
	// that RememberedPreviewDomain from this computer's last deploy.
	PreviewDomain           string
	PreviewDomainSet        bool
	RememberedPreviewDomain string
	Image                   string
	// ExistingKey is reused when it still works for the live API, so other
	// computers configured for this deployment keep working.
	ExistingURL string
	ExistingKey string
	// Confirm is asked before replacing the API key of a live deployment
	// whose key this computer doesn't have. Nil means no.
	Confirm   func(question string) bool
	SkipCheck bool
	Log       io.Writer
}

type Result struct {
	APIURL      string
	APIKey      string
	AccountID   string
	PreviewZone string
}

func Run(ctx context.Context, o Options) (*Result, error) {
	log := func(format string, args ...any) { fmt.Fprintf(o.Log, "  "+format+"\n", args...) }
	cf := cloudflare.New(o.Token)

	npx, err := exec.LookPath("npx")
	if err != nil {
		return nil, errors.New("cloudex deploy runs Cloudflare's wrangler, which needs Node.js 20 or newer (https://nodejs.org)")
	}
	worker, err := assets.ReadFile("assets/worker.js")
	if err != nil {
		return nil, errors.New("this cloudex build has no API bundle; build it with scripts/bundle-api.sh first")
	}
	var meta struct {
		CompatibilityDate  string   `json:"compatibility_date"`
		CompatibilityFlags []string `json:"compatibility_flags"`
	}
	if data, err := assets.ReadFile("assets/meta.json"); err != nil || json.Unmarshal(data, &meta) != nil {
		return nil, errors.New("this cloudex build's API bundle is incomplete; rebuild it with scripts/bundle-api.sh")
	}

	account, err := pickAccount(ctx, cf, o.AccountID)
	if err != nil {
		return nil, err
	}
	log("Account %s (%s)", account.Name, account.ID)
	if err := cf.CheckContainers(ctx, account.ID); err != nil {
		return nil, fmt.Errorf("this account can't run containers yet: %w", err)
	}
	log("Containers are available (Workers Paid)")
	sub, err := cf.WorkersSubdomain(ctx, account.ID)
	if err != nil {
		return nil, err
	}
	if sub == "" {
		return nil, errors.New("this account has no workers.dev subdomain yet: open Workers & Pages in the Cloudflare dashboard once to choose one, then run cloudex deploy again")
	}
	apiURL := fmt.Sprintf("https://%s.%s.workers.dev", WorkerName, sub)

	live, err := inspect(ctx, apiURL, o.ExistingURL, o.ExistingKey)
	if err != nil {
		return nil, err
	}
	key := ""
	switch {
	case live.keyWorks:
		key = o.ExistingKey
	case live.running:
		question := fmt.Sprintf("Cloudex is already deployed at %s, and this computer doesn't have its API key.\n"+
			"Deploying gives it a new key, so other computers using it stop working until they run\n"+
			"cloudex login with the new key. To use it from here instead, run: cloudex login --url %s", apiURL, apiURL)
		if o.Confirm == nil || !o.Confirm(question) {
			return nil, fmt.Errorf("stopped before replacing the API key of %s; run cloudex login --url %s to use it, or cloudex deploy --yes to replace the key", apiURL, apiURL)
		}
		log("Replacing the API key of the existing deployment")
	case o.ExistingKey != "" && o.ExistingURL == apiURL:
		key = o.ExistingKey // the API isn't answering; keep what this computer has
	}

	zone := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(o.PreviewDomain)), ".")
	if !o.PreviewDomainSet {
		zone = firstNonEmpty(live.previewZone, o.RememberedPreviewDomain)
		if live.previewZone != "" {
			log("Keeping preview links on *.%s (--preview-domain '' turns them off)", zone)
		}
	}
	zoneID := ""
	if zone != "" {
		if zoneID, err = cf.ZoneID(ctx, account.ID, zone); err != nil {
			return nil, err
		}
	}

	image, err := copyImage(ctx, cf, account.ID, o.Image, log)
	if err != nil {
		return nil, err
	}

	dbID, created, err := cf.EnsureD1(ctx, account.ID, DatabaseName)
	if err != nil {
		return nil, fmt.Errorf("D1 database: %w", err)
	}
	log("D1 database %s (%s)", DatabaseName, map[bool]string{true: "created", false: "existing"}[created])

	backups := true
	if made, err := cf.EnsureR2Bucket(ctx, account.ID, BucketName); err != nil {
		backups = false
		log("Backups of /home are off: %s", r2Problem(err))
	} else {
		log("R2 bucket %s (%s) for backups of /home", BucketName, map[bool]string{true: "created", false: "existing"}[made])
	}

	if key == "" {
		if key, err = newKey(); err != nil {
			return nil, err
		}
	}

	if err := runWrangler(ctx, npx, o.Token, account.ID, wranglerConfig(meta.CompatibilityDate, meta.CompatibilityFlags, image, dbID, zone, backups), worker, key, log); err != nil {
		return nil, err
	}
	log("API deployed at %s", apiURL)
	if live.running {
		log("Sandboxes keep running. Busy connections (ssh, desktops) move to the new version within about 5 minutes and may drop then; reconnect if one does")
	}

	if zone != "" {
		made, err := cf.EnsureWildcardRecord(ctx, zoneID, zone)
		if err != nil {
			return nil, fmt.Errorf("preview DNS record: %w", err)
		}
		if made {
			log("Added a proxied *.%s record for preview links", zone)
		}
		log("Preview links on *.%s; its other subdomains pass through to their origins", zone)
	}

	res := &Result{APIURL: apiURL, APIKey: key, AccountID: account.ID, PreviewZone: zone}
	client := api.New(apiURL, key)
	if err := waitHealthy(ctx, client); err != nil {
		return res, err
	}
	if o.SkipCheck {
		return res, nil
	}
	return res, checkSandbox(ctx, client, log)
}

func pickAccount(ctx context.Context, cf *cloudflare.Client, want string) (cloudflare.Account, error) {
	accounts, err := cf.Accounts(ctx)
	if err != nil {
		return cloudflare.Account{}, fmt.Errorf("checking the API token: %w", err)
	}
	for _, a := range accounts {
		if a.ID == want {
			return a, nil
		}
	}
	if want != "" {
		return cloudflare.Account{}, fmt.Errorf("the token can't reach account %s", want)
	}
	switch len(accounts) {
	case 0:
		return cloudflare.Account{}, errors.New("the token can't reach any account")
	case 1:
		return accounts[0], nil
	}
	var b strings.Builder
	b.WriteString("the token reaches several accounts; pick one with --account-id:")
	for _, a := range accounts {
		fmt.Fprintf(&b, "\n  %s  %s", a.ID, a.Name)
	}
	return cloudflare.Account{}, errors.New(b.String())
}

type registryKeychain struct{ username, password string }

func (k registryKeychain) Resolve(r authn.Resource) (authn.Authenticator, error) {
	if r.RegistryStr() == registryHost {
		return authn.FromConfig(authn.AuthConfig{Username: k.username, Password: k.password}), nil
	}
	return authn.Anonymous, nil
}

// copyImage copies the published image into the account's registry and returns
// its digest-pinned reference there.
func copyImage(ctx context.Context, cf *cloudflare.Client, account, src string, log func(string, ...any)) (string, error) {
	srcRef, err := name.ParseReference(src)
	if err != nil {
		return "", fmt.Errorf("bad image reference %q: %w", src, err)
	}
	tag := srcRef.Identifier()
	if strings.HasPrefix(tag, "sha256:") {
		tag = "sha-" + strings.TrimPrefix(tag, "sha256:")[:12]
	}
	dst := fmt.Sprintf("%s/%s/%s:%s", registryHost, account, imageRepo, tag)
	user, pass, err := cf.RegistryCredentials(ctx, account, 30)
	if err != nil {
		return "", fmt.Errorf("registry credentials: %w", err)
	}
	dstRef, err := name.ParseReference(dst)
	if err != nil {
		return "", err
	}
	opts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.NewMultiKeychain(registryKeychain{user, pass}, authn.DefaultKeychain)),
		remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"}),
	}
	log("Copying the sandbox image %s into your account (about 700 MB the first time)", src)
	t0 := time.Now()
	// Copy only the linux/amd64 image, even when the source is a multi-platform
	// index or carries attestations.
	desc, err := remote.Get(srcRef, opts...)
	if err != nil {
		return "", fmt.Errorf("reading the sandbox image: %w", err)
	}
	img, err := desc.Image()
	if err != nil {
		return "", fmt.Errorf("the sandbox image has no linux/amd64 variant: %w", err)
	}
	if err := writeImage(ctx, dstRef, img, opts, log); err != nil {
		return "", fmt.Errorf("copying the sandbox image: %w", err)
	}
	digest, err := img.Digest()
	if err != nil {
		return "", err
	}
	log("Image copied in %s", time.Since(t0).Round(time.Second))
	return fmt.Sprintf("%s/%s/%s@%s", registryHost, account, imageRepo, digest), nil
}

// How many times writeImage tries, and its pause after the first failure;
// later pauses grow.
var copyAttempts, copyPause = 3, 5 * time.Second

// writeImage pushes img, trying again when an upload fails partway; layers
// already uploaded are skipped. Twice in a row, an upload to the registry over
// HTTP/2 failed with "tls: bad record MAC" while HTTP/1.1 then worked, so the
// retries use HTTP/1.1.
func writeImage(ctx context.Context, dst name.Reference, img v1.Image, opts []remote.Option, log func(string, ...any)) error {
	for attempt := 1; ; attempt++ {
		err := remote.Write(dst, img, opts...)
		if err == nil || attempt == copyAttempts || ctx.Err() != nil {
			return err
		}
		log("Copying stopped (%v); trying again", err)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt) * copyPause):
		}
		if attempt == 1 {
			opts = append(opts, remote.WithTransport(http1Transport()))
		}
	}
}

func http1Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	t.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	return t
}

// r2Problem explains why backups can't be turned on.
func r2Problem(err error) string {
	var cfErr *cloudflare.Error
	switch {
	case errors.As(err, &cfErr) && cfErr.Code == 10042:
		return "R2 isn't turned on for this account. Turn it on in the Cloudflare dashboard (R2 Object Storage; the free tier is enough), then run cloudex deploy again"
	case errors.As(err, &cfErr) && (cfErr.Status == 403 || cfErr.Code == 10000):
		return "this API token can't use R2. Make a new one with the link in cloudex deploy --help, which now includes R2, then run cloudex deploy again"
	default:
		return err.Error()
	}
}

func wranglerConfig(compatDate string, flags []string, image, dbID, zone string, backups bool) map[string]any {
	cfg := map[string]any{
		"name":                WorkerName,
		"main":                "worker.js",
		"no_bundle":           true,
		"compatibility_date":  compatDate,
		"compatibility_flags": flags,
		"workers_dev":         true,
		"observability":       map[string]any{"enabled": true},
		"containers": []any{map[string]any{
			"class_name":        "Sandbox",
			"scheduling_policy": "durable_object",
			"images":            map[string]any{"base": map[string]any{"image": image}},
		}},
		"durable_objects": map[string]any{"bindings": []any{map[string]any{"name": "SANDBOX", "class_name": "Sandbox"}}},
		"exports":         map[string]any{"Sandbox": map[string]any{"type": "durable-object", "storage": "sqlite"}},
		"d1_databases":    []any{map[string]any{"binding": "DB", "database_name": DatabaseName, "database_id": dbID}},
		"vars":            map[string]any{"PREVIEW_ZONE": zone},
	}
	if zone != "" {
		cfg["routes"] = []any{map[string]any{"pattern": "*." + zone + "/*", "zone_name": zone}}
	}
	if backups {
		cfg["r2_buckets"] = []any{map[string]any{"binding": "BACKUPS", "bucket_name": BucketName}}
	}
	return cfg
}

func runWrangler(ctx context.Context, npx, token, account string, cfg map[string]any, worker []byte, key string, log func(string, ...any)) error {
	dir, err := os.MkdirTemp("", "cloudex-deploy-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	cfgJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	secrets, err := json.Marshal(map[string]string{"ADMIN_KEY": key})
	if err != nil {
		return err
	}
	files := map[string][]byte{"wrangler.json": cfgJSON, "worker.js": worker, "secrets.json": secrets}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			return err
		}
	}
	log("Uploading the API Worker with wrangler %s", wranglerVersion)
	cmd := exec.CommandContext(ctx, npx, "--yes", "wrangler@"+wranglerVersion, "deploy",
		"--config", "wrangler.json", "--secrets-file", "secrets.json", "--containers-rollout", "immediate")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"CLOUDFLARE_API_TOKEN="+token,
		"CLOUDFLARE_ACCOUNT_ID="+account,
		"WRANGLER_SEND_METRICS=false",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) > 25 {
			lines = lines[len(lines)-25:]
		}
		return fmt.Errorf("wrangler deploy failed: %w\n%s", err, strings.Join(lines, "\n"))
	}
	return nil
}

// liveDeployment is what an existing API at the deploy URL tells us.
type liveDeployment struct {
	running     bool
	keyWorks    bool
	previewZone string
}

// inspect asks the API at apiURL, if there is one, for its preview domain and
// whether this computer's key still works there.
func inspect(ctx context.Context, apiURL, existingURL, existingKey string) (liveDeployment, error) {
	var live liveDeployment
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	h, err := api.New(apiURL, "").Health(hctx)
	if err != nil || !h.OK {
		return live, nil // nothing deployed yet, or not answering
	}
	live.running = true
	if h.PreviewZone != nil {
		live.previewZone = *h.PreviewZone
	}
	if existingKey == "" || existingURL != apiURL {
		return live, nil
	}
	_, err = api.New(apiURL, existingKey).List(hctx)
	var apiErr *api.Error
	switch {
	case err == nil:
		live.keyWorks = true
	case errors.As(err, &apiErr) && apiErr.Status == 401:
	default:
		return live, fmt.Errorf("checking the existing deployment: %w", err)
	}
	return live, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func waitHealthy(ctx context.Context, client *api.Client) error {
	deadline := time.Now().Add(2 * time.Minute)
	for {
		hctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		h, err := client.Health(hctx)
		cancel()
		if err == nil && h.OK {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the API didn't answer at %s: %v", client.BaseURL, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// checkSandbox starts, uses and deletes a throwaway sandbox. A newly pushed
// image can take up to 20 minutes to become startable, so it keeps trying.
func checkSandbox(ctx context.Context, client *api.Client, log func(string, ...any)) error {
	suffix := make([]byte, 2)
	rand.Read(suffix)
	name := "deploy-check-" + hex.EncodeToString(suffix)
	idle := 5
	log("Starting a test sandbox (a new image can take several minutes to reach Cloudflare's hosts)")
	deadline := time.Now().Add(30 * time.Minute)
	t0 := time.Now()
	for attempt := 1; ; attempt++ {
		sctx, cancel := context.WithTimeout(ctx, 12*time.Minute)
		s, err := client.Create(sctx, api.CreateRequest{Name: name, Size: "standard-1", IdleMinutes: &idle})
		cancel()
		if err == nil {
			defer client.Delete(context.WithoutCancel(ctx), s.ID)
			res, err := client.Exec(ctx, s.ID, api.ExecRequest{Cmd: "echo ok", TimeoutSec: 60})
			if err != nil || strings.TrimSpace(res.Stdout) != "ok" {
				return fmt.Errorf("the test sandbox started but couldn't run a command: %v", err)
			}
			log("Test sandbox started and ran a command in %s; deleted it", time.Since(t0).Round(time.Second))
			return nil
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) || apiErr.Status != 503 || time.Now().After(deadline) {
			return fmt.Errorf("the test sandbox didn't start: %w", err)
		}
		// A failed start leaves its record behind; clear it before trying again.
		client.Delete(ctx, name)
		log("Not startable yet (attempt %d, %s); trying again", attempt, time.Since(t0).Round(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

func newKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "cxk_" + hex.EncodeToString(b), nil
}
