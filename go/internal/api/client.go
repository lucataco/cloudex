// Package api is a client for a Cloudex deployment's /v1 API.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

// Sandbox mirrors the API's sandbox record (packages/schema).
type Sandbox struct {
	ID          string  `json:"id"`
	Name        *string `json:"name"`
	Size        string  `json:"size"`
	State       string  `json:"state"`
	IdleMinutes int     `json:"idleMinutes"`
	CreatedAt   int64   `json:"createdAt"`
	UpdatedAt   int64   `json:"updatedAt"`
	StartedAt   *int64  `json:"startedAt"`
	StoppedAt   *int64  `json:"stoppedAt"`
	SavedAt     *int64  `json:"savedAt"`
	SavedBytes  *int64  `json:"savedBytes"`
	BackupAt    *int64  `json:"backupAt"`
	BackupBytes *int64  `json:"backupBytes"`
	BackupError *string `json:"backupError"`
	Notice      *string `json:"notice"`
	ForkedFrom  *string `json:"forkedFrom"`
	Error       *string `json:"error"`
}

// Label is the name if there is one, otherwise the id.
func (s *Sandbox) Label() string {
	if s.Name != nil && *s.Name != "" {
		return *s.Name
	}
	return s.ID
}

type CreateRequest struct {
	Name        string `json:"name,omitempty"`
	Size        string `json:"size,omitempty"`
	IdleMinutes *int   `json:"idleMinutes,omitempty"`
}

type ExecRequest struct {
	Cmd        string `json:"cmd"`
	Root       bool   `json:"root,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	TimeoutSec int    `json:"timeoutSec,omitempty"`
}

type ExecResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	TimedOut bool   `json:"timedOut"`
	Ms       int64  `json:"ms"`
}

type Health struct {
	OK          bool    `json:"ok"`
	Version     string  `json:"version"`
	PreviewZone *string `json:"previewZone"`
	Backups     *bool   `json:"backups"` // nil from APIs older than backups
}

// Error is a non-2xx answer from the API.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

type Client struct {
	BaseURL string
	Key     string
	HTTP    *http.Client
}

func New(baseURL, key string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Key: key, HTTP: &http.Client{}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach the Cloudex API at %s: %w", c.BaseURL, err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) != nil || e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
			if e.Error == "" {
				e.Error = res.Status
			}
		}
		return &Error{Status: res.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

func (c *Client) Health(ctx context.Context) (*Health, error) {
	var h Health
	return &h, c.do(ctx, http.MethodGet, "/v1/health", nil, &h)
}

func (c *Client) Create(ctx context.Context, r CreateRequest) (*Sandbox, error) {
	var s Sandbox
	return &s, c.do(ctx, http.MethodPost, "/v1/sandboxes", r, &s)
}

func (c *Client) List(ctx context.Context) ([]Sandbox, error) {
	var s []Sandbox
	return s, c.do(ctx, http.MethodGet, "/v1/sandboxes", nil, &s)
}

func (c *Client) Get(ctx context.Context, ref string) (*Sandbox, error) {
	var s Sandbox
	return &s, c.do(ctx, http.MethodGet, "/v1/sandboxes/"+url.PathEscape(ref), nil, &s)
}

func (c *Client) Stop(ctx context.Context, ref string) (*Sandbox, error) {
	var s Sandbox
	return &s, c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(ref)+"/stop", nil, &s)
}

// Resume starts a stopped sandbox from its save, or with fromBackup, from a
// fresh image with /home restored from the newest backup.
func (c *Client) Resume(ctx context.Context, ref string, fromBackup bool) (*Sandbox, error) {
	var s Sandbox
	var body any
	if fromBackup {
		body = map[string]bool{"fromBackup": true}
	}
	return &s, c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(ref)+"/resume", body, &s)
}

// ForkRequest asks for Count new sandboxes starting from a copy of another's
// disk. Size and IdleMinutes default to the original's.
type ForkRequest struct {
	Name        string `json:"name,omitempty"`
	Count       int    `json:"count,omitempty"`
	Size        string `json:"size,omitempty"`
	IdleMinutes *int   `json:"idleMinutes,omitempty"`
}

// Fork returns the forks, including any that failed to start (state "error").
func (c *Client) Fork(ctx context.Context, ref string, r ForkRequest) ([]Sandbox, error) {
	var forks []Sandbox
	return forks, c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(ref)+"/fork", r, &forks)
}

func (c *Client) Delete(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(ref), nil, nil)
}

func (c *Client) Exec(ctx context.Context, ref string, r ExecRequest) (*ExecResult, error) {
	var out ExecResult
	return &out, c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(ref)+"/exec", r, &out)
}

func (c *Client) Host(ctx context.Context, ref string, port int) (string, error) {
	var out struct {
		URL string `json:"url"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(ref)+"/hosts", map[string]int{"port": port}, &out)
	return out.URL, err
}

// Desktop is where a sandbox's desktop is served: the viewer page is Path on
// guest Port, which DialTCP or a preview link for that port reaches.
type Desktop struct {
	Port      int    `json:"port"`
	Path      string `json:"path"`
	Size      string `json:"size"`
	Installed bool   `json:"installed"` // installed by this call
	Started   bool   `json:"started"`   // started by this call rather than already running
}

// Desktop starts the sandbox's desktop, installing it the first time, or
// resizes a running one. An empty size means the API's default.
func (c *Client) Desktop(ctx context.Context, ref, size string) (*Desktop, error) {
	var d Desktop
	body := struct {
		Size string `json:"size,omitempty"`
	}{size}
	return &d, c.do(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(ref)+"/desktop", body, &d)
}

// StopDesktop stops the desktop and the programs on it.
func (c *Client) StopDesktop(ctx context.Context, ref string) error {
	return c.do(ctx, http.MethodDelete, "/v1/sandboxes/"+url.PathEscape(ref)+"/desktop", nil, nil)
}

// DialTCP opens a WebSocket bridged to a TCP port in the sandbox. For port 22,
// sshKey (an authorized_keys line) is added to the sandbox user's keys first.
func (c *Client) DialTCP(ctx context.Context, ref string, port int, sshKey string) (*websocket.Conn, error) {
	u, err := url.Parse(fmt.Sprintf("%s/v1/sandboxes/%s/tcp/%d", c.BaseURL, url.PathEscape(ref), port))
	if err != nil {
		return nil, err
	}
	u.Scheme = strings.Replace(u.Scheme, "http", "ws", 1)
	if sshKey != "" {
		u.RawQuery = url.Values{"key": {sshKey}}.Encode()
	}
	dialCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	conn, res, err := websocket.Dial(dialCtx, u.String(), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + c.Key}},
	})
	if err != nil {
		if res != nil && res.Body != nil {
			body, _ := io.ReadAll(res.Body)
			if msg := strings.TrimSpace(string(body)); msg != "" {
				var e struct {
					Error string `json:"error"`
				}
				if json.Unmarshal(body, &e) == nil && e.Error != "" {
					msg = e.Error
				}
				return nil, &Error{Status: res.StatusCode, Message: msg}
			}
		}
		return nil, err
	}
	// The API sends frames of up to 256 KiB.
	conn.SetReadLimit(-1)
	return conn, nil
}
