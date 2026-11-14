package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A Docker that keeps containers and images, enough for forks: commit,
// image inspect, list, df and delete, with overlay2's rule that an image
// with a child can't be deleted.
type fakeImg struct {
	id, tag, parent string
	labels          map[string]string
	env             []string
	layers          int
	created         int64
}

type fakeCtr struct {
	id, image string
	labels    map[string]string
	env       []string
	running   bool
	sizeRw    int64
}

type forkDocker struct {
	mu      sync.Mutex
	imgs    map[string]*fakeImg
	ctrs    map[string]*fakeCtr
	commits int
	lastCfg CommitConfig
	n       int
}

func startForkDocker(t *testing.T) (*forkDocker, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "cxf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &forkDocker{imgs: map[string]*fakeImg{}, ctrs: map[string]*fakeCtr{}}
	srv := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return f, sock
}

func matches(labels map[string]string, filters string) bool {
	if filters == "" {
		return true
	}
	var f map[string][]string
	json.Unmarshal([]byte(filters), &f)
	for _, kv := range f["label"] {
		k, v, hasV := strings.Cut(kv, "=")
		got, ok := labels[k]
		if !ok || (hasV && got != v) {
			return false
		}
	}
	return true
}

func (f *forkDocker) img(ref string) *fakeImg {
	if i := f.imgs[ref]; i != nil {
		return i
	}
	for _, i := range f.imgs {
		if i.tag == ref {
			return i
		}
	}
	return nil
}

func (f *forkDocker) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, _ := url.PathUnescape(r.URL.EscapedPath())
	q := r.URL.Query()
	switch {
	case r.Method == "GET" && p == "/info":
		fmt.Fprint(w, `{"ServerVersion":"29.1.0","NCPU":4,"MemTotal":8589934592,"Driver":"overlay2"}`)
	case r.Method == "GET" && p == "/containers/json":
		out := []map[string]any{}
		for _, c := range f.ctrs {
			if matches(c.labels, q.Get("filters")) {
				state := "exited"
				if c.running {
					state = "running"
				}
				out = append(out, map[string]any{"Id": c.id, "State": state, "Labels": c.labels, "ImageID": c.image, "SizeRw": c.sizeRw})
			}
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "GET" && strings.HasPrefix(p, "/containers/") && strings.HasSuffix(p, "/json"):
		c := f.ctrs[strings.TrimSuffix(strings.TrimPrefix(p, "/containers/"), "/json")]
		if c == nil {
			http.Error(w, `{"message":"no such container"}`, 404)
			return
		}
		status := "exited"
		if c.running {
			status = "running"
		}
		json.NewEncoder(w).Encode(map[string]any{"Id": c.id, "Image": c.image, "State": map[string]any{"Running": c.running, "Status": status},
			"Config": map[string]any{"Labels": c.labels, "Env": c.env}})
	case r.Method == "DELETE" && strings.HasPrefix(p, "/containers/"):
		delete(f.ctrs, strings.TrimPrefix(p, "/containers/"))
	case r.Method == "GET" && p == "/networks":
		fmt.Fprint(w, `[]`)
	case r.Method == "POST" && p == "/commit":
		c := f.ctrs[q.Get("container")]
		base := f.imgs[c.image]
		var cfg CommitConfig
		json.NewDecoder(r.Body).Decode(&cfg)
		f.lastCfg = cfg
		f.commits++
		f.n++
		labels := map[string]string{}
		for k, v := range c.labels {
			labels[k] = v
		}
		for k, v := range cfg.Labels {
			labels[k] = v
		}
		id := fmt.Sprintf("sha256:%064d", f.n)
		f.imgs[id] = &fakeImg{id: id, tag: q.Get("repo") + ":" + q.Get("tag"), parent: base.id, labels: labels, layers: base.layers + 1, created: time.Now().Unix()}
		fmt.Fprintf(w, `{"Id":%q}`, id)
	case r.Method == "GET" && p == "/images/json":
		out := []map[string]any{}
		for _, i := range f.imgs {
			if matches(i.labels, q.Get("filters")) {
				out = append(out, map[string]any{"Id": i.id, "Labels": i.labels, "Created": i.created, "Size": 100, "SharedSize": -1})
			}
		}
		json.NewEncoder(w).Encode(out)
	case r.Method == "GET" && p == "/system/df":
		out := []map[string]any{}
		for _, i := range f.imgs {
			out = append(out, map[string]any{"Id": i.id, "Size": 3000 + 7, "SharedSize": 3000})
		}
		json.NewEncoder(w).Encode(map[string]any{"Images": out})
	case r.Method == "GET" && strings.HasPrefix(p, "/images/") && strings.HasSuffix(p, "/json"):
		i := f.img(strings.TrimSuffix(strings.TrimPrefix(p, "/images/"), "/json"))
		if i == nil {
			http.Error(w, `{"message":"no such image"}`, 404)
			return
		}
		layers := make([]string, i.layers)
		json.NewEncoder(w).Encode(map[string]any{"Id": i.id, "Parent": i.parent, "Config": map[string]any{"Labels": i.labels}, "RootFS": map[string]any{"Layers": layers}})
	case r.Method == "DELETE" && strings.HasPrefix(p, "/images/"):
		i := f.img(strings.TrimPrefix(p, "/images/"))
		if i == nil {
			http.Error(w, `{"message":"no such image"}`, 404)
			return
		}
		for _, c := range f.ctrs {
			if c.image == i.id {
				http.Error(w, `{"message":"image is being used by a container"}`, 409)
				return
			}
		}
		for _, o := range f.imgs {
			if o.parent == i.id {
				http.Error(w, `{"message":"image has dependent child images"}`, 409)
				return
			}
		}
		delete(f.imgs, i.id)
	default:
		http.Error(w, `{"message":"not faked: `+r.Method+" "+p+`"}`, 404)
	}
}

const forkNode = "nd-forktest"

func forkRuntime(t *testing.T) (*Runtime, *forkDocker) {
	t.Helper()
	f, sock := startForkDocker(t)
	f.imgs["sha256:base"] = &fakeImg{id: "sha256:base", tag: DefaultImageRepo + ":v0.2.2", labels: map[string]string{"org.opencontainers.image.title": "cloudex-base"}, layers: 11}
	f.ctrs["c-orig"] = &fakeCtr{id: "c-orig", image: "sha256:base", running: true, sizeRw: 1 << 30,
		labels: map[string]string{LabelNode: forkNode, LabelSandbox: "cx-orig01", LabelRole: "machine"},
		env:    []string{"PATH=/usr/bin", "CLOUDEX_SANDBOX=cx-orig01", "CLOUDEX_DOCKER=0", "CLOUDEX_HOLD=1", "LANG=C.UTF-8"}}
	r := &Runtime{Docker: &Docker{Socket: sock}, NodeID: forkNode, Self: "cloudex-node", Forks: NewForkBook(t.TempDir()), FreeSpace: func() float64 { return 100 }}
	return r, f
}

func TestACommitBlanksTheOriginalAndARepeatGetsTheSameImage(t *testing.T) {
	r, f := forkRuntime(t)
	ctx := context.Background()
	point := "cx-orig01-fork-1731600000000"
	info, err := r.Commit(ctx, "cx-orig01", point)
	if err != nil {
		t.Fatal(err)
	}
	if info.Image != "cloudex-fork/"+forkNode+":"+point || !info.Paused || info.Layers != 12 || info.Bytes != 7 {
		t.Fatalf("%+v", info)
	}
	cfg := f.lastCfg
	if strings.Join(cfg.Env, " ") != "CLOUDEX_SANDBOX= CLOUDEX_DOCKER= CLOUDEX_HOLD=" {
		t.Fatalf("env %v: every CLOUDEX_ variable of the original must be blanked, nothing else named", cfg.Env)
	}
	want := map[string]string{LabelNode: forkNode, LabelRole: "fork", LabelSandbox: "", LabelForkOf: "cx-orig01", LabelPoint: point}
	for k, v := range want {
		if cfg.Labels[k] != v {
			t.Fatalf("label %s = %q, want %q (all: %v)", k, cfg.Labels[k], v, cfg.Labels)
		}
	}
	// The image Docker made names the original nowhere but forkof.
	img := f.imgs[info.ID]
	for k, v := range img.labels {
		if strings.Contains(v, "cx-orig01") && k != LabelForkOf && k != LabelPoint {
			t.Fatalf("label %s=%s names the original", k, v)
		}
	}
	again, err := r.Commit(ctx, "cx-orig01", point)
	if err != nil || again.ID != info.ID || f.commits != 1 {
		t.Fatalf("repeat: %+v %v, %d commits; want the same image from one commit", again, err, f.commits)
	}
	if _, err := r.Commit(ctx, "cx-orig01", "cx-other1-fork-1731600000000"); err == nil {
		t.Fatal("a point of another sandbox was taken")
	}
}

func TestACommitPastTheDepthLimitOrUnderTheFloorIsRefused(t *testing.T) {
	r, f := forkRuntime(t)
	f.imgs["sha256:base"].layers = MaxForkLayers
	_, err := r.Commit(context.Background(), "cx-orig01", "cx-orig01-fork-1731600000001")
	if err == nil || !strings.Contains(err.Error(), "121 image layers") || !strings.Contains(err.Error(), "cloudex save cx-orig01") {
		t.Fatalf("err %v; want the depth limit, naming a named save", err)
	}
	f.imgs["sha256:base"].layers = 11
	r.FreeSpace = func() float64 { return 10.5 } // 1 GiB written leaves less than the 10 GB new machines need
	_, err = r.Commit(context.Background(), "cx-orig01", "cx-orig01-fork-1731600000002")
	if err == nil || !strings.Contains(err.Error(), "needs about 1.1 GB") || f.commits != 0 {
		t.Fatalf("err %v, %d commits; want a refusal before any commit", err, f.commits)
	}
}

func TestCreateTakesOnlyThisNodesRecordedForkImages(t *testing.T) {
	r, f := forkRuntime(t)
	ctx := context.Background()
	point := "cx-orig01-fork-1731600000003"
	info, err := r.Commit(ctx, "cx-orig01", point)
	if err != nil {
		t.Fatal(err)
	}
	if id, err := r.forkImage(ctx, info.Image); err != nil || id != info.ID {
		t.Fatalf("%s %v", id, err)
	}
	// Another node's name for it, or a point this node never committed.
	if _, err := r.forkImage(ctx, "cloudex-fork/nd-other000:"+point); err == nil || !strings.Contains(err.Error(), "wasn't made on this node") {
		t.Fatalf("another node's: %v", err)
	}
	f.imgs["sha256:planted"] = &fakeImg{id: "sha256:planted", tag: "cloudex-fork/" + forkNode + ":cx-orig01-fork-1731600000009",
		labels: map[string]string{LabelNode: forkNode, LabelRole: "fork", LabelPoint: "cx-orig01-fork-1731600000009"}, layers: 12}
	if _, err := r.forkImage(ctx, "cloudex-fork/"+forkNode+":cx-orig01-fork-1731600000009"); err == nil || !strings.HasPrefix(err.Error(), ErrForkImageGone) {
		t.Fatalf("an image labelled like a fork but never recorded: %v", err)
	}
	// Relabelled by someone on the host: refused too.
	f.imgs[info.ID].labels[LabelNode] = "nd-other000"
	if _, err := r.forkImage(ctx, info.Image); err == nil {
		t.Fatal("an image whose labels name another node was taken")
	}
}

func TestTheLastMachineOfAForkTakesItsImageAndTheSweepTheRest(t *testing.T) {
	r, f := forkRuntime(t)
	ctx := context.Background()
	p1 := "cx-orig01-fork-1731600000004"
	i1, err := r.Commit(ctx, "cx-orig01", p1)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cx-fork01", "cx-fork02"} {
		f.ctrs["c-"+id] = &fakeCtr{id: "c-" + id, image: i1.ID, labels: map[string]string{LabelNode: forkNode, LabelSandbox: id, LabelRole: "machine"}}
	}
	// A fork of a fork: its image's parent is i1.
	p2 := "cx-fork02-fork-1731600000005"
	i2, err := r.Commit(ctx, "cx-fork02", p2)
	if err != nil {
		t.Fatal(err)
	}
	if rec, _ := r.Forks.Get(p2); rec.Parent != i1.ID {
		t.Fatalf("the record of a fork of a fork has parent %q, want %s", rec.Parent, i1.ID)
	}
	f.ctrs["c-cx-fork03"] = &fakeCtr{id: "c-cx-fork03", image: i2.ID, labels: map[string]string{LabelNode: forkNode, LabelSandbox: "cx-fork03", LabelRole: "machine"}}
	if err := r.Delete(ctx, "cx-fork01"); err != nil {
		t.Fatal(err)
	}
	if f.imgs[i1.ID] == nil {
		t.Fatal("the image went while cx-fork02 still used it")
	}
	if err := r.Delete(ctx, "cx-fork02"); err != nil {
		t.Fatal(err)
	}
	if f.imgs[i1.ID] == nil {
		t.Fatal("the image went while its child image (overlay2) still needed it")
	}
	// The last machine of the child goes: the child, then its parent.
	if err := r.Delete(ctx, "cx-fork03"); err != nil {
		t.Fatal(err)
	}
	if f.imgs[i2.ID] != nil || f.imgs[i1.ID] != nil {
		t.Fatalf("left %v", f.imgs)
	}
	if len(r.Forks.All()) != 0 {
		t.Fatalf("forks.json still has %v", r.Forks.All())
	}
	if f.imgs["sha256:base"] == nil {
		t.Fatal("the base image went")
	}
	// An orphan (its forks never made): the sweep takes it once it's old enough.
	p3 := "cx-orig01-fork-1731600000006"
	i3, err := r.Commit(ctx, "cx-orig01", p3)
	if err != nil {
		t.Fatal(err)
	}
	if n := r.SweepForks(ctx, time.Hour); n != 0 || f.imgs[i3.ID] == nil {
		t.Fatalf("a new fork image was swept (%d)", n)
	}
	if n := r.SweepForks(ctx, 0); n != 1 || f.imgs[i3.ID] != nil {
		t.Fatalf("swept %d; the orphan is still there: %v", n, f.imgs[i3.ID] != nil)
	}
	if fact := r.forkFact(ctx, &DockerInfo{Driver: "overlay2"}); fact.Images != 0 || fact.Store != "overlay2" || fact.SecPerGB != 2.3 {
		t.Fatalf("%+v", fact)
	}
}

func TestImageStoreFromDockerInfo(t *testing.T) {
	cases := map[string]DockerInfo{
		"containerd": {Driver: "overlayfs", DriverStatus: [][2]string{{"driver-type", "io.containerd.snapshotter.v1"}}},
		"overlay2":   {Driver: "overlay2", DriverStatus: [][2]string{{"Backing Filesystem", "extfs"}}},
		"other":      {Driver: "btrfs"},
	}
	for want, i := range cases {
		if got := i.ImageStore(); got != want {
			t.Fatalf("%v: %s, want %s", i, got, want)
		}
	}
}
