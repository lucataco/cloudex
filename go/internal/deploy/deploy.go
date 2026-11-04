// Package deploy installs or updates Cloudex in a Cloudflare account.
//
// The steps, in order:
//  1. Check the token and that the account can run containers (Workers Paid).
//  2. Copy the published sandbox image into the account's own registry. The
//     durable_object scheduling policy only runs digest-pinned images from
//     that registry, and this needs no Docker on the user's machine.
//  3. Find or create the D1 database, and the preview zone's wildcard record.
//  4. Upload the embedded API Worker through the Cloudflare API, the way
//     wrangler deploy does, with the admin key as a secret; give its Durable
//     Objects their container application; turn on its workers.dev URL and
//     the preview route.
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
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/cloudflare"
)

//go:embed assets
var assets embed.FS

// readAsset reads a file of the embedded API bundle; tests replace it, since
// the bundle is built by scripts/bundle-api.sh and isn't in the repository.
var readAsset = assets.ReadFile

// DefaultImage is the published sandbox image. Release builds pin it to their
// version with -ldflags "-X github.com/lucataco/cloudex/go/internal/deploy.DefaultImage=...".
var DefaultImage = "ghcr.io/lucataco/cloudex-base:edge"

const (
	WorkerName   = "cloudex-api"
	BucketName   = "cloudex-backups"
	DatabaseName = "cloudex"
	imageRepo    = "cloudex-base"
	registryHost = "registry.cloudflare.com"
	// The container application of the Sandbox Durable Objects, under the name
	// wrangler gave it, so deployments made with wrangler keep theirs.
	containerName = WorkerName + "-sandbox"
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
	// computers configured for this deployment keep working. It's only tried
	// on the API at ExistingURL, or on one in ExistingAccountID, the same
	// deployment after the account's workers.dev subdomain changed.
	ExistingURL       string
	ExistingKey       string
	ExistingAccountID string
	// Confirm is asked before replacing the API key of a live deployment
	// whose key this computer doesn't have. Nil means no.
	Confirm func(question string) bool
	// OnInstalled is called as soon as the API Worker is installed, with the
	// key it now holds, before the slow steps that follow. An error stops the
	// deploy.
	OnInstalled func(Result) error
	SkipCheck   bool
	Log         io.Writer
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

	worker, err := readAsset("assets/worker.js")
	if err != nil {
		return nil, errors.New("this cloudex build has no API bundle; build it with scripts/bundle-api.sh first")
	}
	var meta struct {
		CompatibilityDate  string   `json:"compatibility_date"`
		CompatibilityFlags []string `json:"compatibility_flags"`
	}
	if data, err := readAsset("assets/meta.json"); err != nil || json.Unmarshal(data, &meta) != nil {
		return nil, errors.New("this cloudex build's API bundle is incomplete; rebuild it with scripts/bundle-api.sh")
	}

	account, err := pickAccount(ctx, cf, o.AccountID)
	if err != nil {
		return nil, err
	}
	log("Account %s (%s)", account.Name, account.ID)
	if err := cf.CheckContainers(ctx, account.ID); err != nil {
		if denied(err) {
			return nil, tokenNeeds("checking that the account can run containers", permContainers, err)
		}
		return nil, fmt.Errorf("this account can't run containers yet: %w", err)
	}
	log("Containers are available (Workers Paid)")
	sub, err := cf.WorkersSubdomain(ctx, account.ID)
	if err != nil {
		return nil, stepError("looking up the workers.dev subdomain", permScripts, err)
	}
	if sub == "" {
		return nil, errors.New("this account has no workers.dev subdomain yet: open Workers & Pages in the Cloudflare dashboard once to choose one, then run cloudex deploy again")
	}
	apiURL := fmt.Sprintf("https://%s.%s.workers.dev", WorkerName, sub)

	ours := o.ExistingURL == apiURL || (o.ExistingAccountID == account.ID && isWorkersDevAPI(o.ExistingURL))
	live, err := inspect(ctx, apiURL, o.ExistingKey, ours)
	if err != nil {
		return nil, err
	}
	key := ""
	switch {
	case live.keyWorks:
		key = o.ExistingKey
		if o.ExistingURL != apiURL {
			log("The account's workers.dev subdomain has changed: the API is now at %s", apiURL)
		}
	case live.running:
		question := fmt.Sprintf("Cloudex is already deployed at %s, and this computer doesn't have its API key.\n"+
			"Deploying gives it a new key, so other computers using it stop working until they run\n"+
			"cloudex login with the new key. %s\n"+
			"To use it from here instead, run: cloudex login --url %s", apiURL, keyReplacedEffects, apiURL)
		if o.Confirm == nil || !o.Confirm(question) {
			return nil, fmt.Errorf("stopped before replacing the API key of %s; run cloudex login --url %s to use it, or cloudex deploy --yes to replace the key", apiURL, apiURL)
		}
		log("Replacing the API key of the existing deployment. %s", keyReplacedEffects)
	case o.ExistingKey != "" && ours:
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
		if zoneID, err = lookupZone(ctx, cf, account.ID, zone); err != nil {
			return nil, err
		}
	}

	image, err := copyImage(ctx, cf, account.ID, o.Image, log)
	if err != nil {
		return nil, err
	}
	if err := prepareImage(ctx, cf, account.ID, image, log); err != nil {
		return nil, err
	}

	dbID, created, err := cf.EnsureD1(ctx, account.ID, DatabaseName)
	if err != nil {
		return nil, stepError("D1 database", permD1, err)
	}
	log("D1 database %s (%s)", DatabaseName, map[bool]string{true: "created", false: "existing"}[created])

	backups := true
	if made, err := cf.EnsureR2Bucket(ctx, account.ID, BucketName); err != nil {
		backups = false
		log("Backups of /home are off: %s", r2Problem(err))
	} else {
		log("R2 bucket %s (%s) for backups and named saves", BucketName, map[bool]string{true: "created", false: "existing"}[made])
	}

	if key == "" {
		if key, err = newKey(); err != nil {
			return nil, err
		}
	}

	if o.PreviewDomainSet {
		// Before the upload: if this fails, the live Worker still names the
		// zone, so running deploy again finds it.
		for _, old := range previousZones(zone, live.previewZone, o.RememberedPreviewDomain) {
			if err := retirePreviews(ctx, cf, account.ID, old, log); err != nil {
				return nil, err
			}
		}
	}

	md := workerMetadata(meta.CompatibilityDate, meta.CompatibilityFlags, image, dbID, zone, key, backups)
	if err := installWorker(ctx, cf, account.ID, md, worker, zoneID, zone, log); err != nil {
		return nil, err
	}
	log("API deployed at %s", apiURL)
	if live.running {
		log("Sandboxes keep running. Busy connections (ssh, desktops) move to the new version within about 5 minutes and may drop then; reconnect if one does")
	}
	res := &Result{APIURL: apiURL, APIKey: key, AccountID: account.ID, PreviewZone: zone}
	if o.OnInstalled != nil {
		if err := o.OnInstalled(*res); err != nil {
			return res, err
		}
	}

	if zone != "" {
		made, err := cf.EnsureWildcardRecord(ctx, zoneID, zone)
		if err != nil {
			return res, stepError("preview DNS record", permDNS, err)
		}
		if made {
			log("Added a proxied *.%s record for preview links", zone)
		}
		log("Preview links on *.%s; its other subdomains pass through to their origins", zone)
	}

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
		return cloudflare.Account{}, stepError("checking the API token", permAccount, err)
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
	e := &accountsError{}
	for _, a := range accounts {
		fmt.Fprintf(&b, "\n  %s  %s", a.ID, a.Name)
		e.names = append(e.names, a.ID, a.Name)
	}
	e.msg = b.String()
	return cloudflare.Account{}, e
}

// accountsError lists the accounts a token reaches, whose names and ids
// cloudex feedback leaves out of a report.
type accountsError struct {
	msg   string
	names []string
}

func (e *accountsError) Error() string           { return e.msg }
func (e *accountsError) PrivateValues() []string { return e.names }

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
		return "", stepError("registry credentials", permContainers, err)
	}
	dstRef, err := name.ParseReference(dst)
	if err != nil {
		return "", err
	}
	platform := remote.WithPlatform(v1.Platform{OS: "linux", Architecture: "amd64"})
	opts := []remote.Option{
		remote.WithContext(ctx),
		remote.WithAuthFromKeychain(authn.NewMultiKeychain(registryKeychain{user, pass}, authn.DefaultKeychain)),
		platform,
	}
	log("Copying the sandbox image %s into your account (about 700 MB the first time)", src)
	t0 := time.Now()
	// Copy only the linux/amd64 image, even when the source is a multi-platform
	// index or carries attestations.
	desc, err := readImage(srcRef, remote.WithContext(ctx), platform)
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

// readImage reads the source image without credentials, so a stale docker
// login can't break a public image. An image that needs a login, such as a
// private one given with --image, is read again with the docker credentials.
func readImage(ref name.Reference, opts ...remote.Option) (*remote.Descriptor, error) {
	desc, err := remote.Get(ref, append(opts, remote.WithAuth(authn.Anonymous))...)
	var terr *transport.Error
	if errors.As(err, &terr) && (terr.StatusCode == http.StatusUnauthorized || terr.StatusCode == http.StatusForbidden) {
		return remote.Get(ref, append(opts, remote.WithAuthFromKeychain(authn.DefaultKeychain))...)
	}
	return desc, err
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
		return "this API token can't use R2. Add the " + permR2 + " permission to it, or make a new token with the link in cloudex deploy --help, then run cloudex deploy again"
	default:
		return err.Error()
	}
}

// Image preparation can take minutes for a new image; wrangler allows 15.
var prepareTimeout, preparePause = 15 * time.Minute, 2 * time.Second

// prepareImage waits until Cloudflare has the image ready to run; a Worker
// version can only name a prepared image.
func prepareImage(ctx context.Context, cf *cloudflare.Client, account, image string, log func(string, ...any)) error {
	deadline := time.Now().Add(prepareTimeout)
	for logged := false; ; {
		status, reason, err := cf.PrepareImage(ctx, account, image)
		switch {
		case err != nil:
			return stepError("preparing the sandbox image", permContainers, err)
		case status == "ready":
			return nil
		case status == "error":
			return fmt.Errorf("Cloudflare couldn't prepare the sandbox image: %s", firstNonEmpty(reason, "no reason given"))
		case status != "pending":
			return fmt.Errorf("preparing the sandbox image: unexpected status %q", status)
		case time.Now().After(deadline):
			return fmt.Errorf("Cloudflare was still preparing the sandbox image after %s; run cloudex deploy again", prepareTimeout)
		}
		if !logged {
			log("Waiting for Cloudflare to prepare the image")
			logged = true
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(preparePause):
		}
	}
}

// installWorker uploads the API Worker, then does what wrangler deploy does
// after an upload: gives the Durable Objects their container application, and
// turns on the workers.dev URL and the preview route.
func installWorker(ctx context.Context, cf *cloudflare.Client, account string, md map[string]any, code []byte, zoneID, zone string, log func(string, ...any)) error {
	log("Uploading the API Worker")
	if err := cf.UploadWorker(ctx, account, WorkerName, md, "worker.js", code); err != nil {
		return stepError("uploading the API Worker", permScripts, err)
	}
	ns, err := cf.DurableObjectNamespace(ctx, account, WorkerName, "Sandbox")
	if err != nil {
		return stepError("reading the API Worker's Durable Objects", permScripts, err)
	}
	made, err := cf.EnsureContainerApplication(ctx, account, containerName, ns)
	if err != nil {
		return stepError("container application", permContainers, err)
	}
	if made {
		log("Created the container application %s", containerName)
	}
	if err := cf.EnableWorkersDev(ctx, account, WorkerName); err != nil {
		return stepError("turning on the workers.dev URL", permScripts, err)
	}
	if zone != "" {
		if err := cf.RouteWorker(ctx, account, WorkerName, zoneID, zone, "*."+zone+"/*"); err != nil {
			return stepError("preview route", permRoutes, err)
		}
	}
	return nil
}

// workerMetadata describes the API Worker for the Workers upload API: what
// wrangler sends for the configuration in apps/api/wrangler.jsonc.
func workerMetadata(compatDate string, flags []string, image, dbID, zone, key string, backups bool) map[string]any {
	if flags == nil {
		flags = []string{}
	}
	bindings := []map[string]any{
		{"type": "durable_object_namespace", "name": "SANDBOX", "class_name": "Sandbox"},
		{"type": "d1", "name": "DB", "id": dbID},
		{"type": "plain_text", "name": "PREVIEW_ZONE", "text": zone},
		{"type": "secret_text", "name": "ADMIN_KEY", "text": key},
	}
	if backups {
		bindings = append(bindings, map[string]any{"type": "r2_bucket", "name": "BACKUPS", "bucket_name": BucketName})
	}
	return map[string]any{
		"main_module":         "worker.js",
		"bindings":            bindings,
		"compatibility_date":  compatDate,
		"compatibility_flags": flags,
		"exports":             map[string]any{"Sandbox": map[string]any{"type": "durable-object", "storage": "sqlite"}},
		"containers": []any{map[string]any{
			"name":       containerName,
			"class_name": "Sandbox",
			"images":     map[string]string{"base": image},
		}},
		"observability": map[string]any{"enabled": true},
	}
}

// liveDeployment is what an existing API at the deploy URL tells us.
type liveDeployment struct {
	running     bool
	keyWorks    bool
	previewZone string
}

// inspect asks the API at apiURL, if there is one, for its preview domain and
// whether this computer's key still works there.
// isWorkersDevAPI says whether url is where deploy puts the API on workers.dev.
func isWorkersDevAPI(url string) bool {
	return strings.HasPrefix(url, "https://"+WorkerName+".") && strings.HasSuffix(url, ".workers.dev")
}

// inspect looks at the API at apiURL, and tries existingKey on it if ours:
// this computer's key goes only to its own deployment.
func inspect(ctx context.Context, apiURL, existingKey string, ours bool) (liveDeployment, error) {
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
	if existingKey == "" || !ours {
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

// The permissions of the token that TokenURL asks for, by the step that needs each.
const (
	permAccount    = "Account Settings Read"
	permScripts    = "Workers Scripts Write"
	permContainers = "Workers Containers Write"
	permD1         = "D1 Write"
	permR2         = "Workers R2 Storage Write"
	permRoutes     = "Workers Routes Write"
	permDNS        = "DNS Write"
	permZone       = "Zone Read"
)

// keyReplacedEffects is what a new API key costs besides the other
// computers: what the old key sealed or signed stops working.
const keyReplacedEffects = "Sandboxes' environment variables and files, the provider keys set with cloudex keys, and preview links stop working too."

// denied is true when Cloudflare refused the token, as opposed to failing.
func denied(err error) bool {
	var cfErr *cloudflare.Error
	return errors.As(err, &cfErr) && (cfErr.Status == http.StatusForbidden || cfErr.Code == 10000)
}

func tokenNeeds(step, permission string, err error) error {
	return fmt.Errorf("%s: this API token isn't allowed to do that. It needs the %s permission. Add it to the token, or make a new token with the link in cloudex deploy --help, then run cloudex deploy again (%w)", step, permission, err)
}

// stepError says what failed, and when Cloudflare refused the token, which
// permission the step needs.
func stepError(step, permission string, err error) error {
	if denied(err) {
		return tokenNeeds(step, permission, err)
	}
	return fmt.Errorf("%s: %w", step, err)
}

func lookupZone(ctx context.Context, cf *cloudflare.Client, account, zone string) (string, error) {
	id, err := cf.ZoneID(ctx, account, zone)
	if denied(err) {
		return "", tokenNeeds("looking up "+zone, permZone, err)
	}
	return id, err
}

// previousZones lists the zones that had preview links, other than the one
// that has them now.
func previousZones(now string, before ...string) []string {
	var zones []string
	for _, z := range before {
		if z != "" && z != now && !slices.Contains(zones, z) {
			zones = append(zones, z)
		}
	}
	return zones
}

// retirePreviews takes preview links off zone: the Worker route, so its
// other subdomains stop reaching the Worker, and the wildcard record Cloudex
// added. A zone the token can't see is left alone.
func retirePreviews(ctx context.Context, cf *cloudflare.Client, account, zone string, log func(string, ...any)) error {
	zoneID, err := lookupZone(ctx, cf, account, zone)
	var noZone *cloudflare.NoZoneError
	if errors.As(err, &noZone) {
		log("Preview links were on %s, which isn't a zone this token can reach; leaving its route and records alone", zone)
		return nil
	}
	if err != nil {
		return err
	}
	removed, err := cf.DeleteWorkerRoutes(ctx, zoneID, WorkerName, "*."+zone+"/*")
	if err != nil {
		return stepError("removing the preview route of "+zone, permRoutes, err)
	}
	if removed {
		log("Removed the preview route *.%s/*; the other subdomains of %s no longer reach the API", zone, zone)
	}
	if deleted, err := cf.DeleteWildcardRecord(ctx, zoneID, zone); err != nil {
		log("Couldn't remove the *.%s record Cloudex added (%v); delete it in the Cloudflare dashboard if you don't need it", zone, err)
	} else if deleted {
		log("Removed the *.%s record Cloudex added", zone)
	}
	return nil
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

// testSandboxName matches the names checkSandbox gives its sandboxes. Each
// lives through one start of up to 12 minutes, so one older than
// leftoverTestSandbox was left by a deploy that was stopped, not one running now.
var testSandboxName = regexp.MustCompile(`^deploy-check-[0-9a-f]{4}$`)

const leftoverTestSandbox = 40 * time.Minute

// checkSandbox starts, uses and deletes a throwaway sandbox. A newly pushed
// image can take up to 20 minutes to become startable, so it keeps trying.
func checkSandbox(ctx context.Context, client *api.Client, log func(string, ...any)) error {
	suffix := make([]byte, 2)
	rand.Read(suffix)
	name := "deploy-check-" + hex.EncodeToString(suffix)
	idle := 5
	if left, err := client.List(ctx); err == nil {
		for _, s := range left {
			if s.Name == nil || !testSandboxName.MatchString(*s.Name) || time.Since(time.UnixMilli(s.CreatedAt)) < leftoverTestSandbox {
				continue
			}
			if client.Delete(ctx, s.ID) == nil {
				log("Deleted %s, left over from an earlier deploy", *s.Name)
			}
		}
	}
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
