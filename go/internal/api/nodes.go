package api

import (
	"context"
	"net/http"
	"net/url"
)

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
	return &n, c.do(ctx, http.MethodPost, "/v1/nodes", map[string]string{"name": name}, &n)
}

func (c *Client) ListNodes(ctx context.Context) ([]Node, error) {
	var ns []Node
	return ns, c.do(ctx, http.MethodGet, "/v1/nodes", nil, &ns)
}

// GetNode takes a name or an id.
func (c *Client) GetNode(ctx context.Context, ref string) (*Node, error) {
	var n Node
	return &n, c.do(ctx, http.MethodGet, "/v1/nodes/"+url.PathEscape(ref), nil, &n)
}

// DeleteNode revokes a node, or forgets one that is revoked or never joined.
// It refuses a node with sandboxes unless force.
func (c *Client) DeleteNode(ctx context.Context, ref string, force bool) (map[string]string, error) {
	path := "/v1/nodes/" + url.PathEscape(ref)
	if force {
		path += "?force=1"
	}
	var out map[string]string
	return out, c.do(ctx, http.MethodDelete, path, nil, &out)
}
