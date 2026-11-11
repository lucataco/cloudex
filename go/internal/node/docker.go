package node

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultDockerSocket is where the Docker Engine API listens.
const DefaultDockerSocket = "/var/run/docker.sock"

// Docker is a small client for the Docker Engine API over its unix socket:
// the calls the agent makes, and nothing that needs the docker CLI.
type Docker struct {
	Socket string
	hc     *http.Client
}

func (d *Docker) socket() string {
	if d.Socket != "" {
		return d.Socket
	}
	return DefaultDockerSocket
}

func (d *Docker) dial(ctx context.Context) (net.Conn, error) {
	var nd net.Dialer
	return nd.DialContext(ctx, "unix", d.socket())
}

func (d *Docker) client() *http.Client {
	if d.hc == nil {
		d.hc = &http.Client{Transport: &http.Transport{
			DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return d.dial(ctx) },
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     30 * time.Second,
		}}
	}
	return d.hc
}

// DockerError is an answer from the daemon that isn't a success.
type DockerError struct {
	Status  int
	Message string
}

func (e *DockerError) Error() string { return fmt.Sprintf("docker: %s (HTTP %d)", e.Message, e.Status) }

// IsNotFound says whether err is the daemon's 404.
func IsNotFound(err error) bool {
	var de *DockerError
	return errors.As(err, &de) && de.Status == http.StatusNotFound
}

func (d *Docker) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := d.client().Do(req)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return &DockerError{Status: res.StatusCode, Message: e.Message}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// Container is what the agent reads of a container.
type Container struct {
	ID      string
	Name    string
	Running bool
	Address string // its IPv4 address on its first network with one
}

func (d *Docker) Inspect(ctx context.Context, name string) (*Container, error) {
	var c struct {
		ID    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Running bool `json:"Running"`
		} `json:"State"`
		NetworkSettings struct {
			IPAddress string `json:"IPAddress"`
			Networks  map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(name)+"/json", nil, &c); err != nil {
		return nil, err
	}
	addr := c.NetworkSettings.IPAddress
	if addr == "" {
		for _, n := range c.NetworkSettings.Networks {
			if n.IPAddress != "" {
				addr = n.IPAddress
				break
			}
		}
	}
	return &Container{ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Running: c.State.Running, Address: addr}, nil
}

// ExecConfig is POST /containers/{id}/exec's body.
type ExecConfig struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
	User         string   `json:"User,omitempty"`
	ConsoleSize  *[2]int  `json:"ConsoleSize,omitempty"` // rows, cols
}

func (d *Docker) ExecCreate(ctx context.Context, container string, cfg ExecConfig) (string, error) {
	var r struct {
		ID string `json:"Id"`
	}
	if err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(container)+"/exec", cfg, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

// Hijacked is a started exec's connection: Reader has what the process
// writes (raw with a TTY, multiplexed without one; see Demux), and writing to
// Conn is its stdin.
type Hijacked struct {
	Conn   net.Conn
	Reader *bufio.Reader
}

// CloseWrite ends the process's stdin.
func (h *Hijacked) CloseWrite() error {
	if cw, ok := h.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// ExecStart starts an exec and hands back its connection, as docker exec -i does.
func (d *Docker) ExecStart(ctx context.Context, id string, tty bool, size *[2]int) (*Hijacked, error) {
	conn, err := d.dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	body, _ := json.Marshal(map[string]any{"Detach": false, "Tty": tty, "ConsoleSize": size})
	req := fmt.Sprintf("POST /exec/%s/start HTTP/1.1\r\nHost: docker\r\nContent-Type: application/json\r\nConnection: Upgrade\r\nUpgrade: tcp\r\nContent-Length: %d\r\n\r\n", url.PathEscape(id), len(body))
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}
	if _, err := conn.Write(append([]byte(req), body...)); err != nil {
		conn.Close()
		return nil, fmt.Errorf("docker: starting the exec: %w", err)
	}
	br := bufio.NewReaderSize(conn, 256<<10)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("docker: starting the exec: %w", err)
	}
	if res.StatusCode != http.StatusSwitchingProtocols && res.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		conn.Close()
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return nil, &DockerError{Status: res.StatusCode, Message: e.Message}
	}
	conn.SetDeadline(time.Time{})
	return &Hijacked{Conn: conn, Reader: br}, nil
}

func (d *Docker) ExecResize(ctx context.Context, id string, cols, rows int) error {
	return d.do(ctx, http.MethodPost, "/exec/"+url.PathEscape(id)+"/resize?h="+strconv.Itoa(rows)+"&w="+strconv.Itoa(cols), nil, nil)
}

// ExecState is what GET /exec/{id}/json says.
type ExecState struct {
	Running  bool `json:"Running"`
	ExitCode *int `json:"ExitCode"`
	Pid      int  `json:"Pid"`
}

func (d *Docker) ExecInspect(ctx context.Context, id string) (*ExecState, error) {
	var s ExecState
	if err := d.do(ctx, http.MethodGet, "/exec/"+url.PathEscape(id)+"/json", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Run runs a command in a container to its end and returns its stdout,
// for the agent's own small jobs (finding and signalling a session).
func (d *Docker) Run(ctx context.Context, container, user string, argv []string) (string, int, error) {
	id, err := d.ExecCreate(ctx, container, ExecConfig{AttachStdout: true, AttachStderr: true, Cmd: argv, User: user})
	if err != nil {
		return "", -1, err
	}
	h, err := d.ExecStart(ctx, id, false, nil)
	if err != nil {
		return "", -1, err
	}
	defer h.Conn.Close()
	var out, errOut bytes.Buffer
	if err := Demux(h.Reader, func(ch int, p []byte) error {
		if ch == ChStdout {
			out.Write(p)
		} else {
			errOut.Write(p)
		}
		return nil
	}); err != nil {
		return "", -1, err
	}
	for i := 0; ; i++ {
		s, err := d.ExecInspect(ctx, id)
		if err != nil {
			return "", -1, err
		}
		if !s.Running && s.ExitCode != nil {
			return out.String(), *s.ExitCode, nil
		}
		if err := sleepCtx(ctx, 5*time.Millisecond); err != nil {
			return "", -1, err
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Demux reads Docker's multiplexed stream (an exec without a TTY): frames of
// an 8-byte header (the stream, 1 stdout or 2 stderr, then a big-endian
// length) and that many bytes. It calls emit with the channel (ChStdout or
// ChStderr) and the bytes, coalescing frames of one channel that are already
// buffered, up to 256 KiB, so that many small writes become few frames.
func Demux(r *bufio.Reader, emit func(ch int, p []byte) error) error {
	const most = 256 << 10
	var hdr [8]byte
	buf := make([]byte, 0, most)
	cur := -1
	flush := func() error {
		if len(buf) == 0 {
			return nil
		}
		err := emit(cur, buf)
		buf = make([]byte, 0, most)
		return err
	}
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if ferr := flush(); ferr != nil {
				return ferr
			}
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		ch := ChStdout
		if hdr[0] == 2 {
			ch = ChStderr
		}
		n := int(binary.BigEndian.Uint32(hdr[4:]))
		if ch != cur || len(buf)+n > most {
			if err := flush(); err != nil {
				return err
			}
			cur = ch
		}
		for n > 0 {
			take := min(n, most-len(buf))
			if take == 0 {
				if err := flush(); err != nil {
					return err
				}
				cur = ch
				continue
			}
			start := len(buf)
			buf = buf[:start+take]
			if _, err := io.ReadFull(r, buf[start:]); err != nil {
				buf = buf[:start]
				flush()
				return nil
			}
			n -= take
		}
		// Only what's already here joins this frame.
		if r.Buffered() < 8 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
}
