package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultStateDir is where the agent keeps its key and identity: a Docker
// volume when it runs as the cloudex-node container. --state-dir or
// CLOUDEX_NODE_STATE put it elsewhere, for a plain process or a test.
const DefaultStateDir = "/var/lib/cloudex-node"

// StateDir picks the state directory: the flag, else the environment, else the default.
func StateDir(flag string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv("CLOUDEX_NODE_STATE"); env != "" {
		return env
	}
	return DefaultStateDir
}

// Identity is node.json: who this node is, once it has joined.
type Identity struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	APIURL     string `json:"apiUrl"`
	Unisolated bool   `json:"unisolated,omitempty"`
	JoinedAt   int64  `json:"joinedAt"`
}

// State is the state directory: node.key (the Ed25519 seed in hex, 0600),
// node.json and status.json (what the running agent last saw, for status).
type State struct{ Dir string }

func (s State) path(name string) string { return filepath.Join(s.Dir, name) }

// Key reads the node's key, or makes and saves one when there is none yet.
func (s State) Key() (ed25519.PrivateKey, error) {
	b, err := os.ReadFile(s.path("node.key"))
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(s.Dir, 0o700); err != nil {
			return nil, err
		}
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		// O_EXCL: two agents starting at once don't each write a key.
		f, err := os.OpenFile(s.path("node.key"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, fs.ErrExist) {
				return s.Key()
			}
			return nil, err
		}
		defer f.Close()
		if _, err := f.WriteString(hex.EncodeToString(seed) + "\n"); err != nil {
			return nil, err
		}
		return ed25519.NewKeyFromSeed(seed), nil
	}
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(s.path("node.key")); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s can be read by other users (mode %o); chmod 600 it", s.path("node.key"), fi.Mode().Perm())
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s isn't a node key (a hex Ed25519 seed)", s.path("node.key"))
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Identity reads node.json; nil when the node hasn't joined.
func (s State) Identity() (*Identity, error) {
	b, err := os.ReadFile(s.path("node.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil || id.ID == "" || id.APIURL == "" {
		return nil, fmt.Errorf("%s is damaged; remove the state directory and join again with a new token", s.path("node.json"))
	}
	return &id, nil
}

func (s State) write(name string, v any, mode os.FileMode) error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(name + ".tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), mode); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(name))
}

func (s State) SaveIdentity(id Identity) error { return s.write("node.json", id, 0o600) }

// Status is status.json, which the running agent rewrites as its connection
// comes and goes, and cloudex-node status reads.
type Status struct {
	PID        int    `json:"pid"`
	Connected  bool   `json:"connected"`
	Since      int64  `json:"since"`      // ms: when it connected, or went down
	LastPongAt int64  `json:"lastPongAt"` // ms
	Attempts   int    `json:"attempts"`   // failed attempts since the last connection
	LastError  string `json:"lastError"`  // why the last attempt or connection ended
	UpdatedAt  int64  `json:"updatedAt"`  // ms
	APIURL     string `json:"apiUrl"`
	NodeID     string `json:"nodeId"`
	Version    string `json:"version"`
	// The last self-test's result (M4).
	Isolation *Isolation `json:"isolation,omitempty"`
	// Revoked: the deployment said this node was removed (review R3).
	Revoked bool `json:"revoked,omitempty"`
}

// LastSubnet is the machine network handed out last (netalloc.go).
func (s State) LastSubnet() netip.Prefix {
	var v struct {
		Subnet string `json:"subnet"`
	}
	if b, err := os.ReadFile(s.path("subnet.json")); err == nil {
		json.Unmarshal(b, &v)
	}
	p, _ := netip.ParsePrefix(v.Subnet)
	return p
}

func (s State) SaveLastSubnet(p netip.Prefix) error {
	if s.Dir == "" {
		return nil
	}
	return s.write("subnet.json", map[string]string{"subnet": p.String()}, 0o600)
}

func (s State) SaveStatus(st Status) error {
	st.UpdatedAt = time.Now().UnixMilli()
	return s.write("status.json", st, 0o644)
}

func (s State) ReadStatus() (*Status, error) {
	b, err := os.ReadFile(s.path("status.json"))
	if err != nil {
		return nil, err
	}
	var st Status
	return &st, json.Unmarshal(b, &st)
}

// Image is the machine image the deployment last named (image.json), so
// that a restarted agent can test and pull before it connects.
func (s State) Image() string {
	var v struct {
		Image string `json:"image"`
	}
	if b, err := os.ReadFile(s.path("image.json")); err == nil {
		json.Unmarshal(b, &v)
	}
	return v.Image
}

func (s State) SaveImage(ref string) error {
	if s.Dir == "" {
		return nil
	}
	return s.write("image.json", map[string]string{"image": ref}, 0o600)
}

func readFile(path string) ([]byte, error) { return os.ReadFile(path) }

// RuntimeConfig is how the agent runs machines (cloudex-node run's flags),
// kept in runtime.json so that cloudex-node selftest, run in the agent's
// container, uses the same.
type RuntimeConfig struct {
	Pool       string   `json:"pool"`
	Self       string   `json:"self"`
	Allowed    []string `json:"allowed,omitempty"`
	Image      string   `json:"image,omitempty"`
	DockerRoot string   `json:"dockerRoot,omitempty"`
	Probes     []string `json:"probes,omitempty"`
	KeepFreeGB float64  `json:"keepFreeGb,omitempty"` // the disk watchdog's floor (guard.go)
	DiskGB     int      `json:"diskGb,omitempty"`     // a machine's writable layer cap, where the driver has quotas
}

func (s State) SaveRuntimeConfig(c RuntimeConfig) error {
	if s.Dir == "" {
		return nil
	}
	return s.write("runtime.json", c, 0o600)
}

func (s State) RuntimeConfig() (*RuntimeConfig, error) {
	b, err := os.ReadFile(s.path("runtime.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c RuntimeConfig
	return &c, json.Unmarshal(b, &c)
}

// Runtime checks the configuration and makes the runtime.
func (c RuntimeConfig) Runtime(d *Docker, lg *log.Logger) (*Runtime, error) {
	pool, err := netip.ParsePrefix(c.Pool)
	if err != nil {
		return nil, fmt.Errorf("--pool: %w", err)
	}
	if err := CheckPool(pool); err != nil {
		return nil, err
	}
	allowed := append([]string{DefaultImageRepo}, c.Allowed...)
	for _, a := range allowed {
		if !imageRef.MatchString(a) || ImageRepo(a) != a {
			return nil, fmt.Errorf("--allow-image %q: give a repository without a tag, such as %s", a, DefaultImageRepo)
		}
	}
	if c.Image != "" {
		if err := ImageAllowed(c.Image, allowed); err != nil {
			return nil, fmt.Errorf("--machine-image: %w", err)
		}
	}
	for _, p := range c.Probes {
		host := strings.TrimPrefix(p, "udp53=")
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if _, err := netip.ParseAddr(host); err != nil {
			return nil, fmt.Errorf("--probe %q: give ip:port, or udp53=ip", p)
		}
	}
	if c.KeepFreeGB < 0 || c.DiskGB < 0 {
		return nil, errors.New("--keep-free-gb and --disk-gb can't be negative")
	}
	return &Runtime{Docker: d, Pool: pool, Self: c.Self, Allowed: allowed, ImageOverride: c.Image, DockerRoot: c.DockerRoot, HostProbes: c.Probes, Log: lg,
		KeepFreeGB: c.KeepFreeGB, DiskGB: c.DiskGB}, nil
}
