package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The Engine API calls the Docker runtime makes (v0.2.2 M4), besides exec
// (docker.go). Every object the agent makes carries its labels, and it lists,
// changes and removes only objects with them; it never prunes.

// Labels on everything the agent makes.
const (
	LabelNode    = "build.cloudex.node"    // the node's id
	LabelSandbox = "build.cloudex.sandbox" // the sandbox a machine or its network is for
	LabelRole    = "build.cloudex.role"    // machine, probe or check
)

// DockerInfo is what GET /info says that the agent uses.
type DockerInfo struct {
	ServerVersion string                     `json:"ServerVersion"`
	NCPU          int                        `json:"NCPU"`
	MemTotal      int64                      `json:"MemTotal"`
	DockerRootDir string                     `json:"DockerRootDir"`
	Runtimes      map[string]json.RawMessage `json:"Runtimes"`
	SecurityOpts  []string                   `json:"SecurityOptions"`
}

func (d *Docker) Info(ctx context.Context) (*DockerInfo, error) {
	var i DockerInfo
	if err := d.do(ctx, http.MethodGet, "/info", nil, &i); err != nil {
		return nil, err
	}
	return &i, nil
}

// HasImage says whether the daemon has an image.
func (d *Docker) HasImage(ctx context.Context, ref string) (bool, error) {
	err := d.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil)
	if IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

// Pull pulls an image, calling progress with the share of its bytes so far.
func (d *Docker) Pull(ctx context.Context, ref string, progress func(float64)) error {
	q := url.Values{}
	if repo, digest, ok := strings.Cut(ref, "@"); ok {
		// repo:tag@digest pulls by digest.
		if i := strings.LastIndex(repo, ":"); i > strings.LastIndex(repo, "/") {
			repo = repo[:i]
		}
		q.Set("fromImage", repo+"@"+digest)
	} else {
		q.Set("fromImage", ref)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://docker/images/create?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	res, err := d.client().Do(req)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return &DockerError{Status: res.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	type layer struct{ cur, total int64 }
	layers := map[string]*layer{}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m struct {
			ID             string `json:"id"`
			Status         string `json:"status"`
			Error          string `json:"error"`
			ProgressDetail struct {
				Current int64 `json:"current"`
				Total   int64 `json:"total"`
			} `json:"progressDetail"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Error != "" {
			return fmt.Errorf("docker: pulling %s: %s", ref, m.Error)
		}
		if m.ID == "" {
			continue
		}
		l := layers[m.ID]
		if l == nil {
			l = &layer{}
			layers[m.ID] = l
		}
		switch {
		case m.Status == "Downloading" && m.ProgressDetail.Total > 0:
			l.cur, l.total = m.ProgressDetail.Current, m.ProgressDetail.Total
		case m.Status == "Download complete" || m.Status == "Pull complete" || m.Status == "Already exists":
			if l.total == 0 {
				l.total = 1
			}
			l.cur = l.total
		}
		if progress != nil {
			var cur, total int64
			for _, l := range layers {
				cur, total = cur+l.cur, total+l.total
			}
			if total > 0 {
				progress(float64(cur) / float64(total))
			}
		}
	}
	return sc.Err()
}

// NetworkCreate is POST /networks/create's body, as the agent fills it.
type NetworkCreate struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Internal   bool              `json:"Internal"`
	Attachable bool              `json:"Attachable"`
	EnableIPv6 bool              `json:"EnableIPv6"`
	IPAM       IPAM              `json:"IPAM"`
	Options    map[string]string `json:"Options"`
	Labels     map[string]string `json:"Labels"`
}

type IPAM struct {
	Driver string       `json:"Driver"`
	Config []IPAMConfig `json:"Config"`
}

type IPAMConfig struct {
	Subnet  string `json:"Subnet"`
	Gateway string `json:"Gateway"`
}

func (d *Docker) NetworkCreate(ctx context.Context, n NetworkCreate) (string, error) {
	var r struct {
		ID string `json:"Id"`
	}
	if err := d.do(ctx, http.MethodPost, "/networks/create", n, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

func (d *Docker) NetworkRemove(ctx context.Context, id string) error {
	err := d.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(id), nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// NetworkConnect attaches a container at a fixed address. A negative
// gwPriority keeps the network from becoming the container's default route.
func (d *Docker) NetworkConnect(ctx context.Context, network, container, ip string, gwPriority int) error {
	body := map[string]any{
		"Container": container,
		"EndpointConfig": map[string]any{
			"IPAMConfig": map[string]string{"IPv4Address": ip},
			"GwPriority": gwPriority,
		},
	}
	return d.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", body, nil)
}

func (d *Docker) NetworkDisconnect(ctx context.Context, network, container string) error {
	err := d.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/disconnect", map[string]any{"Container": container, "Force": true}, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// NetworkSummary is what the agent reads of a network.
type NetworkSummary struct {
	ID      string            `json:"Id"`
	Name    string            `json:"Name"`
	Labels  map[string]string `json:"Labels"`
	Options map[string]string `json:"Options"`
	IPAM    IPAM              `json:"IPAM"`
}

// Networks lists networks; with labels, only those carrying all of them (k=v, or k).
func (d *Docker) Networks(ctx context.Context, labels ...string) ([]NetworkSummary, error) {
	var ns []NetworkSummary
	err := d.do(ctx, http.MethodGet, "/networks"+labelFilter(labels), nil, &ns)
	return ns, err
}

func labelFilter(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	f, _ := json.Marshal(map[string][]string{"label": labels})
	return "?filters=" + url.QueryEscape(string(f))
}

// ContainerSummary is what the agent reads of a container in a list.
type ContainerSummary struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	State  string            `json:"State"` // running, exited, created, ...
	Labels map[string]string `json:"Labels"`
}

// Containers lists containers, stopped ones too, carrying all the labels.
func (d *Docker) Containers(ctx context.Context, labels ...string) ([]ContainerSummary, error) {
	q := "?all=1"
	if f := labelFilter(labels); f != "" {
		q += "&" + strings.TrimPrefix(f, "?")
	}
	var cs []ContainerSummary
	err := d.do(ctx, http.MethodGet, "/containers/json"+q, nil, &cs)
	return cs, err
}

// ContainerCreate makes a container from a body built by MachineSpec (spec.go).
func (d *Docker) ContainerCreate(ctx context.Context, name string, body *CreateBody) (string, error) {
	var r struct {
		ID string `json:"Id"`
	}
	if err := d.do(ctx, http.MethodPost, "/containers/create?name="+url.QueryEscape(name), body, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

func (d *Docker) ContainerStart(ctx context.Context, id string) error {
	err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil)
	var de *DockerError
	if errors.As(err, &de) && de.Status == http.StatusNotModified {
		return nil // running already
	}
	return err
}

// ContainerStop stops a container, giving it t seconds after SIGTERM.
func (d *Docker) ContainerStop(ctx context.Context, id string, t int) error {
	err := d.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop?t="+strconv.Itoa(t), nil, nil)
	var de *DockerError
	if errors.As(err, &de) && de.Status == http.StatusNotModified {
		return nil
	}
	return err
}

// ContainerRemove removes a container and its anonymous volumes.
func (d *Docker) ContainerRemove(ctx context.Context, id string) error {
	err := d.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id)+"?force=1&v=1", nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// ContainerDetail is what the agent reads of one container.
type ContainerDetail struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	State struct {
		Running   bool   `json:"Running"`
		Status    string `json:"Status"`
		OOMKilled bool   `json:"OOMKilled"`
		ExitCode  int    `json:"ExitCode"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
		Image  string            `json:"Image"`
	} `json:"Config"`
	HostConfig      json.RawMessage `json:"HostConfig"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
			NetworkID string `json:"NetworkID"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

func (d *Docker) ContainerInspect(ctx context.Context, id string) (*ContainerDetail, error) {
	var c ContainerDetail
	if err := d.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// Event is one of Docker's events, as the agent reads them.
type Event struct {
	Type   string `json:"Type"`
	Action string `json:"Action"`
	Actor  struct {
		ID         string            `json:"ID"`
		Attributes map[string]string `json:"Attributes"`
	} `json:"Actor"`
	Time int64 `json:"time"`
}

// Events follows the daemon's container events for objects carrying the
// labels until ctx ends or the stream breaks.
func (d *Docker) Events(ctx context.Context, fn func(Event), labels ...string) error {
	f, _ := json.Marshal(map[string][]string{"type": {"container"}, "label": labels, "event": {"start", "die", "oom", "destroy"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/events?filters="+url.QueryEscape(string(f)), nil)
	if err != nil {
		return err
	}
	// The shared client's transport has no response timeout; events stay open.
	res, err := d.client().Do(req)
	if err != nil {
		return fmt.Errorf("docker: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return &DockerError{Status: res.StatusCode, Message: strings.TrimSpace(string(data))}
	}
	dec := json.NewDecoder(res.Body)
	for {
		var e Event
		if err := dec.Decode(&e); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		fn(e)
	}
}
