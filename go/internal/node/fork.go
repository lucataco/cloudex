package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Forks on nodes (v0.2.3 M5). A fork point is one image: a docker commit of
// the original's machine, running (paused for the copy) or stopped, named
// cloudex-fork/<node id>:<point>, where point is "<original>-fork-<ms>" as on
// Cloudflare. The 1-10 forks of one request are machines made from it, each
// with an empty writable layer of its own, so the node keeps one copy of the
// original's changes however many forks share it.
//
//   - The commit's config can only blank or overwrite what the container had
//     (Docker merges it, M0 Q4): the original's CLOUDEX_* variables are set
//     empty, and its build.cloudex.sandbox label too, so nothing in the image
//     names it except build.cloudex.forkof. /run is a tmpfs, so its boot id,
//     self token and markers aren't in the image.
//   - forks.json in the state volume records each point: its image, and
//     whether the commit finished. A repeated commit op (its answer lost to a
//     deploy) gets the same image, or waits for the commit under way.
//   - Create takes a fork image only when it carries this node's labels and
//     forks.json has its point (D15), whatever --allow-image says, and
//     --machine-image doesn't replace it.
//   - Delete removes the fork images its machines used once nothing uses them
//     (by id: a delete by tag only untags), then their parents that are fork
//     images too. A 409 (a container, or on overlay2 a child image, still uses
//     it) is left to the sweep, which runs at start and hourly and removes
//     fork images no container uses that are over an hour old.

// Labels on a fork image, besides build.cloudex.node and build.cloudex.role=fork.
const (
	LabelForkOf = "build.cloudex.forkof" // the sandbox it was forked from
	LabelPoint  = "build.cloudex.point"  // its fork point, which is its tag
)

// ForkRepoPrefix starts every fork image's repository: cloudex-fork/<node id>.
const ForkRepoPrefix = "cloudex-fork/"

// MaxForkLayers is the most layers a fork image may have. overlay2 refuses a
// commit at 126 and the containerd store fails at 256 (M0 Q6); cloudex-base
// has 11, and each fork of a fork adds one.
const MaxForkLayers = 120

// CommitTimeout is how long a commit may take (as SAVE_TIMEOUT_MS on Cloudflare).
var CommitTimeout = 15 * time.Minute

// ForkSweepAge is how old an unused fork image must be before the sweep removes it.
var ForkSweepAge = time.Hour

// ForkSweepEvery is how often the sweep runs.
var ForkSweepEvery = time.Hour

var forkPointPattern = regexp.MustCompile(`^(cx-[a-z0-9]{6})-fork-[0-9]{10,16}$`)

// Seconds per GiB of a sandbox's changes that a commit takes (and a running
// original is paused for), by image store, until this node has measured its
// own: M0 Q4 on a native VM, and N9 for other drivers.
var defaultSecPerGB = map[string]float64{"overlay2": 2.3, "containerd": 12.5, "other": 4.1}

// ImageStore says how Docker keeps images: "containerd" (Docker 29's
// default for new installs: the containerd snapshotter, which keeps a fork
// image about twice, compressed and unpacked), "overlay2" (the classic
// store), or "other" (btrfs, zfs, vfs).
func (i *DockerInfo) ImageStore() string {
	for _, kv := range i.DriverStatus {
		if kv[0] == "driver-type" && strings.HasPrefix(kv[1], "io.containerd.snapshotter") {
			return "containerd"
		}
	}
	if i.Driver == "overlay2" {
		return "overlay2"
	}
	return "other"
}

// ForkFact is what node info shows about fork images (facts.forks).
type ForkFact struct {
	Images   int     `json:"images"`
	GB       float64 `json:"gb"`
	Store    string  `json:"store"`    // overlay2, containerd or other
	SecPerGB float64 `json:"secPerGb"` // a commit's (and the pause's) seconds per GiB of changes, here
}

// ForkInfo answers the commit op.
type ForkInfo struct {
	Image  string `json:"image"` // cloudex-fork/<node>:<point>
	ID     string `json:"id"`    // the image's id
	Bytes  int64  `json:"bytes"` // its own size on disk
	TookMs int64  `json:"tookMs"`
	Paused bool   `json:"paused"` // the original was running, and paused for TookMs
	Layers int    `json:"layers"`
}

// ForkRecord is one point in forks.json.
type ForkRecord struct {
	Image   string `json:"image,omitempty"` // the image's id
	Ref     string `json:"ref"`
	Sandbox string `json:"sandbox"`
	Parent  string `json:"parent,omitempty"` // the original's image, when that's a fork image too
	State   string `json:"state"`            // committing, done or failed
	At      int64  `json:"at"`               // ms: the commit began, then ended
	Bytes   int64  `json:"bytes,omitempty"`
	TookMs  int64  `json:"tookMs,omitempty"`
	Paused  bool   `json:"paused,omitempty"`
	Layers  int    `json:"layers,omitempty"`
}

type forkFile struct {
	Points map[string]ForkRecord `json:"points"`
	// SecPerGB is this node's own measure of a commit's speed, from commits
	// of at least 256 MiB of changes; 0 until there's one.
	SecPerGB float64 `json:"secPerGb,omitempty"`
}

// ForkBook is forks.json. Without a path (tests) it lives in memory.
type ForkBook struct {
	Path string
	mu   sync.Mutex
	mem  *forkFile
}

// NewForkBook is the book in a state directory.
func NewForkBook(dir string) *ForkBook {
	if dir == "" {
		return &ForkBook{}
	}
	return &ForkBook{Path: filepath.Join(dir, "forks.json")}
}

func (b *ForkBook) read() *forkFile {
	if b.Path == "" {
		if b.mem == nil {
			b.mem = &forkFile{Points: map[string]ForkRecord{}}
		}
		return b.mem
	}
	f := &forkFile{}
	// Missing or unreadable: start over. The sweep finds images by their labels.
	if data, err := os.ReadFile(b.Path); err == nil {
		json.Unmarshal(data, f)
	}
	if f.Points == nil {
		f.Points = map[string]ForkRecord{}
	}
	return f
}

func (b *ForkBook) write(f *forkFile) error {
	if b.Path == "" {
		b.mem = f
		return nil
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := b.Path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, b.Path)
}

// Get is a point's record.
func (b *ForkBook) Get(point string) (ForkRecord, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.read().Points[point]
	return r, ok
}

// Put records a point.
func (b *ForkBook) Put(point string, rec ForkRecord) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := b.read()
	f.Points[point] = rec
	return b.write(f)
}

// Forget drops points.
func (b *ForkBook) Forget(points ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := b.read()
	n := len(f.Points)
	for _, p := range points {
		delete(f.Points, p)
	}
	if len(f.Points) != n {
		b.write(f)
	}
}

// All is every point.
func (b *ForkBook) All() map[string]ForkRecord {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.read().Points
}

// Rate is the measured seconds per GiB, 0 when there's none.
func (b *ForkBook) Rate() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.read().SecPerGB
}

// measured folds a commit's speed into the node's measure.
func (b *ForkBook) measured(secPerGB float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	f := b.read()
	if f.SecPerGB == 0 {
		f.SecPerGB = secPerGB
	} else {
		f.SecPerGB = (f.SecPerGB + secPerGB) / 2
	}
	f.SecPerGB = float64(int(f.SecPerGB*10+0.5)) / 10
	b.write(f)
}

// commitRun is a commit under way, which a repeated op waits for.
type commitRun struct {
	done chan struct{}
	info *ForkInfo
	err  error
}

// IsForkRef says whether an image reference names a fork image.
func IsForkRef(ref string) bool { return strings.HasPrefix(ref, ForkRepoPrefix) }

func (r *Runtime) forkRepo() string { return ForkRepoPrefix + r.NodeID }

// isForkImage says whether an image is one of this node's fork images.
func (r *Runtime) isForkImage(img *ImageDetail) bool {
	return img != nil && img.Config.Labels[LabelRole] == "fork" && r.NodeID != "" && img.Config.Labels[LabelNode] == r.NodeID
}

// secPerGB is this node's commit speed: measured, else its store's default.
func (r *Runtime) secPerGB(store string) float64 {
	if r.Forks != nil {
		if v := r.Forks.Rate(); v > 0 {
			return v
		}
	}
	return defaultSecPerGB[store]
}

// Commit makes the image of a fork point of a sandbox's machine (op commit).
func (r *Runtime) Commit(ctx context.Context, sandbox, point string) (*ForkInfo, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	if !sandboxID.MatchString(sandbox) {
		return nil, errors.New("not a sandbox id")
	}
	if m := forkPointPattern.FindStringSubmatch(point); m == nil || m[1] != sandbox {
		return nil, errors.New("not a fork point of that sandbox")
	}
	if r.Forks == nil {
		return nil, errors.New("this agent keeps no record of fork images, so it can't make forks")
	}
	ref := r.forkRepo() + ":" + point
	if rec, ok := r.Forks.Get(point); ok && rec.State == "done" {
		if img, err := r.Docker.ImageInspect(ctx, rec.Image); err == nil && r.isForkImage(img) {
			return &ForkInfo{Image: ref, ID: rec.Image, Bytes: rec.Bytes, TookMs: rec.TookMs, Paused: rec.Paused, Layers: rec.Layers}, nil
		}
	}
	r.forkMu.Lock()
	if run := r.commits[point]; run != nil {
		r.forkMu.Unlock()
		select {
		case <-run.done:
			return run.info, run.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	run := &commitRun{done: make(chan struct{})}
	if r.commits == nil {
		r.commits = map[string]*commitRun{}
	}
	r.commits[point] = run
	r.forkMu.Unlock()
	run.info, run.err = r.commit(ctx, sandbox, point, ref)
	close(run.done)
	r.forkMu.Lock()
	delete(r.commits, point)
	r.forkMu.Unlock()
	return run.info, run.err
}

func (r *Runtime) commit(ctx context.Context, sandbox, point, ref string) (*ForkInfo, error) {
	c, err := r.container(ctx, sandbox)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, fmt.Errorf("no machine here runs %s", sandbox)
	}
	switch c.State.Status {
	case "running", "exited", "created":
	default:
		return nil, fmt.Errorf("%s's machine is %s; try again in a moment", sandbox, c.State.Status)
	}
	base, err := r.Docker.ImageInspect(ctx, c.Image)
	if err != nil {
		return nil, fmt.Errorf("the image %s's machine runs: %w", sandbox, err)
	}
	layers := len(base.RootFS.Layers) + 1
	if layers > MaxForkLayers {
		return nil, fmt.Errorf("a fork of %s would have %d image layers, more than the %d a node allows (each fork of a fork adds one); make a named save of it (cloudex save %s <name>) and start from that (cloudex new --from <name> --on <node>), which starts flat", sandbox, layers, MaxForkLayers, sandbox)
	}
	info, _ := r.dockerInfo(ctx)
	store := "other"
	if info != nil {
		store = info.ImageStore()
	}
	var written int64
	if cs, err := r.Docker.ContainersSized(ctx, r.labels(LabelSandbox+"="+sandbox, LabelRole+"=machine")...); err == nil && len(cs) > 0 {
		written = cs[0].SizeRw
	}
	expected := written
	if store == "containerd" {
		expected *= 2 // a compressed blob and an unpacked snapshot (M0 Q4)
	}
	release, err := r.reserveCommit(point, sandbox, expected)
	if err != nil {
		return nil, err
	}
	defer release()
	var env []string
	for _, kv := range c.Config.Env {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "CLOUDEX_") || k == "CX_EXEC" {
			env = append(env, k+"=")
		}
	}
	labels := map[string]string{LabelNode: r.NodeID, LabelRole: "fork", LabelSandbox: "", LabelForkOf: sandbox, LabelPoint: point}
	parent := ""
	if r.isForkImage(base) {
		parent = base.ID
	}
	running := c.State.Running
	now := time.Now()
	r.Forks.Put(point, ForkRecord{Ref: ref, Sandbox: sandbox, Parent: parent, State: "committing", At: now.UnixMilli()})
	cctx, cancel := context.WithTimeout(ctx, CommitTimeout)
	defer cancel()
	id, err := r.Docker.Commit(cctx, c.ID, r.forkRepo(), point, true, CommitConfig{Env: env, Labels: labels})
	took := time.Since(now)
	if err != nil {
		r.Forks.Put(point, ForkRecord{Ref: ref, Sandbox: sandbox, Parent: parent, State: "failed", At: time.Now().UnixMilli()})
		return nil, fmt.Errorf("copying %s's disk: %w", sandbox, err)
	}
	bytes := expected
	if imgs, err := r.Docker.ImageUsage(ctx); err == nil {
		for _, i := range imgs {
			if i.ID == id && i.SharedSize >= 0 {
				bytes = i.Size - i.SharedSize
			}
		}
	}
	rec := ForkRecord{Image: id, Ref: ref, Sandbox: sandbox, Parent: parent, State: "done", At: time.Now().UnixMilli(), Bytes: bytes, TookMs: took.Milliseconds(), Paused: running, Layers: layers}
	if err := r.Forks.Put(point, rec); err != nil {
		r.logf("recording the fork point %s: %v", point, err)
	}
	if written >= 256<<20 {
		r.Forks.measured(took.Seconds() / (float64(written) / (1 << 30)))
	}
	what := "stopped"
	if running {
		what = "running, paused for the copy"
	}
	r.logf("%s: committed fork point %s in %v (%s; %.2f GB written, image %.2f GB, %d layers, %s store)", sandbox, point, took.Round(10*time.Millisecond), what, float64(written)/1e9, float64(bytes)/1e9, layers, store)
	r.changed()
	return &ForkInfo{Image: ref, ID: id, Bytes: bytes, TookMs: took.Milliseconds(), Paused: running, Layers: layers}, nil
}

// reserveCommit checks that the node's free space, less what the commits
// under way expect to take, leaves the floor after this one's copy, and
// holds expected bytes for it until release. Two forks at once used to pass
// the check each alone, and the node ended under its floor (review F-M1).
func (r *Runtime) reserveCommit(point, sandbox string, expected int64) (release func(), err error) {
	r.forkMu.Lock()
	defer r.forkMu.Unlock()
	var others int64
	for _, b := range r.reserved {
		others += b
	}
	if gb := r.FreeGB(); gb >= 0 && gb-float64(others+expected)/1e9 < r.minFree() {
		if others > 0 && gb-float64(expected)/1e9 >= r.minFree() {
			what := "another fork being copied now takes"
			if len(r.reserved) > 1 {
				what = fmt.Sprintf("%d other forks being copied now take", len(r.reserved))
			}
			return nil, fmt.Errorf("Docker's disk on this node has %.1f GB free, but %s about %.1f GB of it; a fork of %s needs about %.1f GB for its copy and %.0f GB left over. Try again when the copy under way finishes", gb, what, float64(others)/1e9, sandbox, float64(expected)/1e9, r.minFree())
		}
		return nil, fmt.Errorf("Docker's disk on this node has %.1f GB free; a fork of %s needs about %.1f GB for its copy and %.0f GB left over. Free space on the host, or delete sandboxes or forks on the node", gb, sandbox, float64(expected)/1e9, r.minFree())
	}
	if r.reserved == nil {
		r.reserved = map[string]int64{}
	}
	r.reserved[point] = expected
	return func() {
		r.forkMu.Lock()
		delete(r.reserved, point)
		r.forkMu.Unlock()
	}, nil
}

// ErrForkImageGone starts the error create gives for a fork image that's
// gone; the Worker then starts the sandbox from the base image.
const ErrForkImageGone = "the fork image"

// forkImage resolves a fork image's reference to its id, for a create: it
// must be this node's, with its point in forks.json (D15).
func (r *Runtime) forkImage(ctx context.Context, ref string) (string, error) {
	repo, point, _ := strings.Cut(ref, ":")
	gone := fmt.Errorf("%s %s is gone from this node (deleted with the last sandbox that used it, or swept)", ErrForkImageGone, ref)
	if repo != r.forkRepo() || !forkPointPattern.MatchString(point) {
		return "", fmt.Errorf("%s %s wasn't made on this node", ErrForkImageGone, ref)
	}
	if r.Forks == nil {
		return "", gone
	}
	rec, ok := r.Forks.Get(point)
	if !ok || rec.State != "done" || rec.Ref != ref || rec.Image == "" {
		return "", gone
	}
	img, err := r.Docker.ImageInspect(ctx, rec.Image)
	if IsNotFound(err) {
		return "", gone
	} else if err != nil {
		return "", err
	}
	if !r.isForkImage(img) || img.Config.Labels[LabelPoint] != point {
		return "", gone
	}
	return img.ID, nil
}

// dropForkImage removes a fork image once nothing uses it, then its parent
// fork images the same way. Called with r.mu held, after a delete removed
// the containers that used it.
func (r *Runtime) dropForkImage(ctx context.Context, id string) bool {
	dropped := false
	for depth := 0; id != "" && depth <= MaxForkLayers; depth++ {
		img, err := r.Docker.ImageInspect(ctx, id)
		if err != nil || !r.isForkImage(img) {
			return dropped
		}
		point := img.Config.Labels[LabelPoint]
		parent := img.Parent
		if r.Forks != nil {
			if rec, ok := r.Forks.Get(point); ok && rec.Parent != "" {
				parent = rec.Parent
			}
		}
		if err := r.Docker.ImageRemove(ctx, img.ID); err != nil {
			if !IsConflict(err) {
				r.logf("removing the fork image %s: %v", point, err)
			}
			return dropped // still used: a container, or a fork of it (overlay2); the sweep tries again
		}
		dropped = true
		if r.Forks != nil {
			r.Forks.Forget(point)
		}
		r.logf("removed the fork image of %s, which nothing uses now", point)
		id = parent
	}
	return dropped
}

// SweepForks removes this node's fork images that no container uses and
// that are older than minAge, newest first (a fork of a fork before the
// image it came from), but none whose commit is under way. It also forgets
// points whose image is gone. Returns how many it removed.
func (r *Runtime) SweepForks(ctx context.Context, minAge time.Duration) int {
	if r.NodeID == "" {
		return 0
	}
	r.mu.Lock()
	n := r.sweepForks(ctx, minAge)
	r.mu.Unlock()
	if n > 0 {
		r.changed()
	}
	return n
}

func (r *Runtime) sweepForks(ctx context.Context, minAge time.Duration) int {
	imgs, err := r.Docker.Images(ctx, r.labels(LabelRole+"=fork")...)
	if err != nil {
		r.logf("the fork sweep: listing images: %v", err)
		return 0
	}
	cs, err := r.Docker.Containers(ctx, LabelNode+"="+r.NodeID)
	if err != nil {
		r.logf("the fork sweep: listing containers: %v", err)
		return 0
	}
	used := map[string]bool{}
	for _, c := range cs {
		used[c.ImageID] = true
	}
	r.forkMu.Lock()
	busy := map[string]bool{}
	for p := range r.commits {
		busy[p] = true
	}
	r.forkMu.Unlock()
	var book map[string]ForkRecord
	if r.Forks != nil {
		book = r.Forks.All()
	}
	present := map[string]bool{}
	for _, i := range imgs {
		present[i.Labels[LabelPoint]] = true
	}
	now := time.Now()
	// Forget points whose image is gone and whose commit isn't under way.
	var stale []string
	for p, rec := range book {
		if present[p] || busy[p] {
			continue
		}
		if rec.State == "committing" && now.Sub(time.UnixMilli(rec.At)) < 2*CommitTimeout {
			continue
		}
		stale = append(stale, p)
	}
	if len(stale) > 0 && r.Forks != nil {
		r.Forks.Forget(stale...)
	}
	sort.Slice(imgs, func(i, j int) bool { return imgs[i].Created > imgs[j].Created })
	removed := 0
	for pass := 0; pass < 4; pass++ {
		progress := false
		for _, i := range imgs {
			p := i.Labels[LabelPoint]
			if i.ID == "" || used[i.ID] || busy[p] || !present[p] {
				continue
			}
			at := time.Unix(i.Created, 0)
			if rec, ok := book[p]; ok {
				if rec.State == "committing" && now.Sub(time.UnixMilli(rec.At)) < 2*CommitTimeout {
					continue
				}
				if rec.At > 0 {
					at = time.UnixMilli(rec.At)
				}
			}
			if now.Sub(at) < minAge {
				continue
			}
			if err := r.Docker.ImageRemove(ctx, i.ID); err != nil {
				if !IsConflict(err) {
					r.logf("the fork sweep: removing %s: %v", p, err)
				}
				continue
			}
			present[p] = false
			removed++
			progress = true
			if r.Forks != nil {
				r.Forks.Forget(p)
			}
			r.logf("the fork sweep removed the image of %s, which no sandbox uses", p)
		}
		if !progress {
			break
		}
	}
	return removed
}

// forkFact counts this node's fork images, for node info.
func (r *Runtime) forkFact(ctx context.Context, info *DockerInfo) *ForkFact {
	if r.NodeID == "" || info == nil {
		return nil
	}
	store := info.ImageStore()
	f := &ForkFact{Store: store, SecPerGB: r.secPerGB(store)}
	imgs, err := r.Docker.Images(ctx, r.labels(LabelRole+"=fork")...)
	if err != nil {
		return f
	}
	var book map[string]ForkRecord
	if r.Forks != nil {
		book = r.Forks.All()
	}
	var bytes int64
	for _, i := range imgs {
		f.Images++
		if rec, ok := book[i.Labels[LabelPoint]]; ok && rec.Bytes > 0 {
			bytes += rec.Bytes
		}
	}
	f.GB = float64(int(float64(bytes)/1e8+0.5)) / 10
	return f
}

// forkGB is how much fork images hold, for the disk watchdog's notice.
func (r *Runtime) forkGB() float64 {
	if r.Forks == nil {
		return 0
	}
	var bytes int64
	for _, rec := range r.Forks.All() {
		if rec.State == "done" {
			bytes += rec.Bytes
		}
	}
	return float64(bytes) / 1e9
}
