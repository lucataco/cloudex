package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"
)

// ExecEvent is one line of a streamed exec: Type is stdout or stderr with
// Data, exit with ExitCode, TimedOut and Ms, or error with Error. keepalive
// lines, while the command is quiet, aren't passed on.
type ExecEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	ExitCode int    `json:"exitCode"`
	TimedOut bool   `json:"timedOut"`
	Ms       int64  `json:"ms"`
	Error    string `json:"error,omitempty"`
}

// StartCommandRequest starts a background command.
type StartCommandRequest struct {
	Cmd        string `json:"cmd"`
	Root       bool   `json:"root,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	TimeoutSec int    `json:"timeoutSec,omitempty"` // 0: no limit
}

// Command is a background command (Command in packages/schema).
type Command struct {
	ID          string  `json:"id"`
	Cmd         string  `json:"cmd"`
	Root        bool    `json:"root"`
	Cwd         string  `json:"cwd"`
	State       string  `json:"state"` // running, exited, killed or lost
	ExitCode    *int    `json:"exitCode"`
	Signal      *string `json:"signal"`
	TimedOut    bool    `json:"timedOut"`
	TimeoutSec  int     `json:"timeoutSec"`
	StartedAt   int64   `json:"startedAt"`
	EndedAt     *int64  `json:"endedAt"`
	OutputBytes int64   `json:"outputBytes"`
}

// CommandOutput is the part of a command's log from byte Offset to Next.
type CommandOutput struct {
	Data    string  `json:"data"`
	Offset  int64   `json:"offset"`
	Next    int64   `json:"next"`
	Command Command `json:"command"`
}

// FileEntry is one entry in a directory.
type FileEntry struct {
	Name   string `json:"name"`
	Type   string `json:"type"` // file, dir, symlink or other
	Size   int64  `json:"size"`
	Mode   string `json:"mode"` // octal, such as 644
	Mtime  int64  `json:"mtime"`
	Target string `json:"target,omitempty"`
}

// FileInfo is a path's details and, for a directory, its entries.
type FileInfo struct {
	Path      string      `json:"path"`
	Type      string      `json:"type"`
	Size      int64       `json:"size"`
	Mode      string      `json:"mode"`
	Mtime     int64       `json:"mtime"`
	Target    string      `json:"target,omitempty"`
	Entries   []FileEntry `json:"entries,omitempty"`
	Truncated bool        `json:"truncated,omitempty"`
}

// ArchiveResult is where an uploaded archive went.
type ArchiveResult struct {
	Path    string `json:"path"`
	Entries int    `json:"entries"`
}

// request sends a request with a raw body and returns the response when it
// succeeded; the caller closes its body.
func (c *Client) request(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	// A file's size is known, so it goes with a Content-Length rather than chunked.
	if f, ok := body.(*os.File); ok {
		if info, err := f.Stat(); err == nil && info.Mode().IsRegular() {
			if pos, err := f.Seek(0, io.SeekCurrent); err == nil {
				req.ContentLength = info.Size() - pos
				if req.ContentLength == 0 {
					req.Body = http.NoBody
				}
			}
		}
	}
	if c.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.Key)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("can't reach the Cloudex API at %s: %w", c.BaseURL, err)
	}
	if res.StatusCode >= 300 {
		defer res.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		return nil, c.failure(ctx, res, data)
	}
	return res, nil
}

// ExecStream runs a command and calls on with each piece of output as it
// comes, then returns how it ended. An API from before streaming answers all
// at once, which comes out the same way.
func (c *Client) ExecStream(ctx context.Context, ref string, r ExecRequest, on func(ExecEvent) error) (*ExecEvent, error) {
	body := struct {
		ExecRequest
		Stream bool `json:"stream"`
	}{r, true}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	res, err := c.request(ctx, http.MethodPost, "/v1/sandboxes/"+url.PathEscape(ref)+"/exec", bytesReader(data), "application/json")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt != "application/x-ndjson" {
		var all ExecResult
		if err := json.NewDecoder(res.Body).Decode(&all); err != nil {
			return nil, err
		}
		for _, ev := range []ExecEvent{{Type: "stdout", Data: all.Stdout}, {Type: "stderr", Data: all.Stderr}} {
			if ev.Data != "" {
				if err := on(ev); err != nil {
					return nil, err
				}
			}
		}
		return &ExecEvent{Type: "exit", ExitCode: all.ExitCode, TimedOut: all.TimedOut, Ms: all.Ms}, nil
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	for sc.Scan() {
		var ev ExecEvent
		if len(sc.Bytes()) == 0 {
			continue
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("unexpected line from the API: %.200s", sc.Text())
		}
		switch ev.Type {
		case "exit":
			return &ev, nil
		case "error":
			return nil, errors.New(ev.Error)
		case "keepalive":
		default:
			if err := on(ev); err != nil {
				return nil, err
			}
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("the connection to the sandbox broke off: %w", err)
	}
	return nil, errors.New("the connection to the sandbox broke off before the command ended")
}

func commandsPath(ref string) string {
	return "/v1/sandboxes/" + url.PathEscape(ref) + "/commands"
}

func (c *Client) StartCommand(ctx context.Context, ref string, r StartCommandRequest) (*Command, error) {
	var cmd Command
	return &cmd, c.do(ctx, http.MethodPost, commandsPath(ref), r, &cmd)
}

func (c *Client) ListCommands(ctx context.Context, ref string) ([]Command, error) {
	var list []Command
	return list, c.do(ctx, http.MethodGet, commandsPath(ref), nil, &list)
}

func (c *Client) GetCommand(ctx context.Context, ref, id string) (*Command, error) {
	var cmd Command
	return &cmd, c.do(ctx, http.MethodGet, commandsPath(ref)+"/"+url.PathEscape(id), nil, &cmd)
}

// CommandOutput returns the command's output from byte after. With wait (at
// most 60 seconds), the API answers as soon as there's more, the command
// ends, or the seconds pass.
func (c *Client) CommandOutput(ctx context.Context, ref, id string, after int64, wait int) (*CommandOutput, error) {
	var out CommandOutput
	q := url.Values{"after": {strconv.FormatInt(after, 10)}, "wait": {strconv.Itoa(wait)}}
	return &out, c.do(ctx, http.MethodGet, commandsPath(ref)+"/"+url.PathEscape(id)+"/output?"+q.Encode(), nil, &out)
}

func (c *Client) KillCommand(ctx context.Context, ref, id, signal string) (*Command, error) {
	var cmd Command
	return &cmd, c.do(ctx, http.MethodPost, commandsPath(ref)+"/"+url.PathEscape(id)+"/kill", map[string]string{"signal": signal}, &cmd)
}

// ForgetCommand deletes a finished command's record and output.
func (c *Client) ForgetCommand(ctx context.Context, ref, id string) (*Command, error) {
	var cmd Command
	return &cmd, c.do(ctx, http.MethodDelete, commandsPath(ref)+"/"+url.PathEscape(id), nil, &cmd)
}

func filesPath(ref, sub, path string, root bool, extra url.Values) string {
	q := url.Values{"path": {path}}
	if root {
		q.Set("root", "true")
	}
	for k, v := range extra {
		q[k] = v
	}
	return "/v1/sandboxes/" + url.PathEscape(ref) + "/files" + sub + "?" + q.Encode()
}

// Stat returns a path's details and, for a directory, what's in it.
func (c *Client) Stat(ctx context.Context, ref, path string, root bool) (*FileInfo, error) {
	var fi FileInfo
	return &fi, c.do(ctx, http.MethodGet, filesPath(ref, "", path, root, nil), nil, &fi)
}

func (c *Client) DeleteFile(ctx context.Context, ref, path string, root, recursive bool) (*FileInfo, error) {
	var fi FileInfo
	extra := url.Values{}
	if recursive {
		extra.Set("recursive", "true")
	}
	return &fi, c.do(ctx, http.MethodDelete, filesPath(ref, "", path, root, extra), nil, &fi)
}

// ReadFile streams a file's contents; the caller closes it.
func (c *Client) ReadFile(ctx context.Context, ref, path string, root bool) (io.ReadCloser, error) {
	res, err := c.request(ctx, http.MethodGet, filesPath(ref, "/content", path, root, nil), nil, "")
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// WriteFile uploads a file; mode is octal, or empty to keep the file's own.
func (c *Client) WriteFile(ctx context.Context, ref, path string, root bool, mode string, body io.Reader) (*FileInfo, error) {
	extra := url.Values{}
	if mode != "" {
		extra.Set("mode", mode)
	}
	res, err := c.request(ctx, http.MethodPut, filesPath(ref, "/content", path, root, extra), body, "application/octet-stream")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var fi FileInfo
	return &fi, json.NewDecoder(res.Body).Decode(&fi)
}

// DownloadArchive streams a tar.gz of a directory's contents, or of one file.
func (c *Client) DownloadArchive(ctx context.Context, ref, path string, root bool) (io.ReadCloser, error) {
	res, err := c.request(ctx, http.MethodGet, filesPath(ref, "/archive", path, root, nil), nil, "")
	if err != nil {
		return nil, err
	}
	return res.Body, nil
}

// UploadArchive unpacks a tar.gz into a directory, making it if need be.
func (c *Client) UploadArchive(ctx context.Context, ref, path string, root bool, body io.Reader) (*ArchiveResult, error) {
	res, err := c.request(ctx, http.MethodPut, filesPath(ref, "/archive", path, root, nil), body, "application/gzip")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var ar ArchiveResult
	return &ar, json.NewDecoder(res.Body).Decode(&ar)
}

func bytesReader(b []byte) io.Reader { return bytes.NewReader(b) }
