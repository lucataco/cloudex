package api

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// ForceNodeTimeout is what cloudex node rm --force waits beyond ShortTimeout:
// an online node's machines are removed first, each within 90 s.
var ForceNodeTimeout = 2 * time.Minute

// Node is a user's own host that runs sandboxes (packages/schema/src/node.ts).
type Node struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	State        string         `json:"state"` // pending, online, offline or revoked
	CreatedAt    int64          `json:"createdAt"`
	LastSeenAt   *int64         `json:"lastSeenAt"`
	Version      *string        `json:"version"`
	Protocol     *int           `json:"protocol"`
	Arch         *string        `json:"arch"`
	Runtime      *string        `json:"runtime"` // docker or sysbox
	Isolation    *NodeIsolation `json:"isolation"`
	CPUs         *int           `json:"cpus"`
	MemoryMB     *int           `json:"memoryMb"`
	DiskFreeGB   *float64       `json:"diskFreeGb"`
	Colo         *string        `json:"colo"`
	Continent    *string        `json:"continent"`
	LocationHint *string        `json:"locationHint"`
	Machines     int            `json:"machines"`
	RevokedAt    *int64         `json:"revokedAt"`
	RTTMs        *int64         `json:"rttMs,omitempty"`
	Image        *NodeImage     `json:"image,omitempty"`
	// Direct is the node's direct port (v0.2.3): absent for an older agent
	// or with the port turned off.
	Direct *NodeDirect `json:"direct,omitempty"`
	// PublicKey is the node's Ed25519 key in base64 (v0.2.3), which its
	// direct port's certificate carries: absent before it joins, once it's
	// removed, and from an older deployment.
	PublicKey string `json:"publicKey,omitempty"`
	// GetNode only (v0.2.2 M5): machines of no sandbox, sandboxes deleted
	// while it was offline whose machines it removes when it's back, and its
	// agent's clock less the deployment's at its last connect.
	Orphans        []string `json:"orphans,omitempty"`
	PendingDeletes []string `json:"pendingDeletes,omitempty"`
	ClockSkewSec   *int     `json:"clockSkewSec,omitempty"`
}

type NodeIsolation struct {
	Firewall   string   `json:"firewall"` // pass, fail or unchecked
	Unisolated bool     `json:"unisolated"`
	CheckedAt  *int64   `json:"checkedAt,omitempty"`
	Failed     []string `json:"failed,omitempty"`
}

// NodeDirect is a node's direct port, as its agent reports it.
type NodeDirect struct {
	Port  int      `json:"port"`
	Addrs []string `json:"addrs"`
	State string   `json:"state"` // listening, off or failed
	Error string   `json:"error,omitempty"`
}

// Direct is GET /v1/sandboxes/{ref}/direct's answer (v0.2.3): where the
// sandbox runs and, for a node with a usable direct port, the node's key to
// pin, its candidates and a ticket for one connection.
type Direct struct {
	Sandbox  string `json:"sandbox"`
	Provider string `json:"provider"` // cloudflare or node
	Node     *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"node,omitempty"`
	PublicKey string `json:"publicKey,omitempty"`
	Direct    *struct {
		Port  int      `json:"port"`
		Addrs []string `json:"addrs"`
	} `json:"direct,omitempty"`
	Ticket    *string `json:"ticket,omitempty"`
	ExpiresAt *int64  `json:"expiresAt,omitempty"`
}

// Direct asks for a direct ticket to a sandbox's port; key, when given, is
// the ssh public key to authorize in it.
func (c *Client) Direct(ctx context.Context, ref string, port int, key string) (*Direct, error) {
	q := url.Values{"port": {strconv.Itoa(port)}}
	if key != "" {
		q.Set("key", key)
	}
	var d Direct
	return &d, c.doShort(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(ref)+"/direct?"+q.Encode(), nil, &d)
}

// NodeImage is the machine image on a node, and whether it has it.
type NodeImage struct {
	Ref      string   `json:"ref"`
	State    string   `json:"state"` // present, pulling, missing or refused
	Progress *float64 `json:"progress,omitempty"`
}

// NewNode is a node as added: the token its agent joins with, once, before
// ExpiresAt, and the command that installs the agent.
type NewNode struct {
	Node      Node   `json:"node"`
	JoinToken string `json:"joinToken"`
	ExpiresAt int64  `json:"expiresAt"`
	Install   string `json:"install"`
}

func (c *Client) CreateNode(ctx context.Context, name string) (*NewNode, error) {
	var n NewNode
	return &n, c.doShort(ctx, http.MethodPost, "/v1/nodes", map[string]string{"name": name}, &n)
}

func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	var ns []Node
	return ns, c.doShort(ctx, http.MethodGet, "/v1/nodes", nil, &ns)
}

// GetNode takes a name or an id.
func (c *Client) GetNode(ctx context.Context, ref string) (*Node, error) {
	var n Node
	return &n, c.doShort(ctx, http.MethodGet, "/v1/nodes/"+url.PathEscape(ref), nil, &n)
}

// DeleteNode revokes a node, or forgets one that is revoked or never joined.
// It refuses a node with sandboxes unless force.
func (c *Client) DeleteNode(ctx context.Context, ref string, force bool) (map[string]string, error) {
	path := "/v1/nodes/" + url.PathEscape(ref)
	if force {
		path += "?force=1"
	}
	var out map[string]string
	if !force {
		return out, c.doShort(ctx, http.MethodDelete, path, nil, &out)
	}
	// With force, an online node first removes its sandboxes' machines,
	// each within 90 s (index.ts), so that has longer (review R2).
	return out, c.doWithin(ctx, ShortTimeout+ForceNodeTimeout, http.MethodDelete, path, nil, &out)
}
