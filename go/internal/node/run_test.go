package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Review H1: a machine's root can replace the commands the agent runs in it
// (the ready check, the session scan and kill). Run must neither keep what
// such a command prints without limit nor wait for one that never ends.
func TestRunStopsAtItsOutputCap(t *testing.T) {
	f, sock := startFakeDocker(t)
	d := &Docker{Socket: sock}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	t0 := time.Now()
	out, code, err := d.Run(ctx, "m", "0", []string{"yes"})
	if !errors.Is(err, ErrRunOutput) || out != "" || code != -1 {
		t.Fatalf("got %d bytes, exit %d, %v; want ErrRunOutput", len(out), code, err)
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatalf("took %v", time.Since(t0))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cfg.Env) != 1 || f.cfg.Env[0] != RunPath {
		t.Fatalf("env %v; want only %s", f.cfg.Env, RunPath)
	}
}

func TestRunEndsWithItsContext(t *testing.T) {
	_, sock := startFakeDocker(t)
	d := &Docker{Socket: sock}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, _, err := d.Run(ctx, "m", "0", []string{"hang"})
	if err == nil || time.Since(t0) > 3*time.Second {
		t.Fatalf("err %v after %v; want an error within the deadline", err, time.Since(t0))
	}
}

func TestRunWithoutADeadlineGetsOne(t *testing.T) {
	old := RunTimeout
	defer func() { RunTimeout = old }()
	RunTimeout = 300 * time.Millisecond
	_, sock := startFakeDocker(t)
	d := &Docker{Socket: sock}
	t0 := time.Now()
	if _, _, err := d.Run(context.Background(), "m", "0", []string{"hang"}); err == nil || time.Since(t0) > 3*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(t0))
	}
}

func TestRunStillReturnsAnOrdinaryAnswer(t *testing.T) {
	_, sock := startFakeDocker(t)
	d := &Docker{Socket: sock}
	out, code, err := d.Run(context.Background(), "m", "0", []string{"echo", "ab", "3"})
	if err != nil || out != "ababab" || code != 3 {
		t.Fatalf("%q %d %v", out, code, err)
	}
}

func TestAStartGivesUpAtOnceOnAPlantedSh(t *testing.T) {
	// The ready check prints nothing: endless output means the machine's
	// root replaced /bin/sh, and asking again every 100 ms for 90 s would
	// only pile its execs up in Docker.
	f, sock := startFakeDocker(t)
	r := &Runtime{Docker: &Docker{Socket: sock}}
	t0 := time.Now()
	err := r.waitReady(context.Background(), "m", 90*time.Second)
	if err == nil || !strings.Contains(err.Error(), "ready check printed more than 64 KiB") || time.Since(t0) > 5*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(t0))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cfg.Cmd[0] != "/bin/sh" {
		t.Fatalf("ran %v", f.cfg.Cmd)
	}
}

func TestReattachJoinsTheNetworksTheAgentLost(t *testing.T) {
	// The installer replaced the agent's container: it is on cloudex-node
	// and one machine network, and must join the other one again.
	dir, err := os.MkdirTemp("", "cxd") // a unix socket's path must be short
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var joined []string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/containers/cloudex-node/json"):
			io.WriteString(w, `{"Id":"self","NetworkSettings":{"Networks":{"cloudex-node":{},"cloudex-cx-aaa111":{}}}}`)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/connect"):
			var b struct {
				Container      string
				EndpointConfig struct{ IPAMConfig struct{ IPv4Address string } }
			}
			json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			joined = append(joined, r.URL.Path+" "+b.Container+" "+b.EndpointConfig.IPAMConfig.IPv4Address)
			mu.Unlock()
		default:
			http.NotFound(w, r)
		}
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	nets := map[string]MachineNet{}
	for id, sub := range map[string]string{"cx-aaa111": "10.250.0.0/28", "cx-bbb222": "10.250.0.16/28"} {
		n, err := NetFor(id, netip.MustParsePrefix(sub))
		if err != nil {
			t.Fatal(err)
		}
		nets[id] = n
	}
	r := &Runtime{Docker: &Docker{Socket: sock}, Self: "cloudex-node"}
	r.Reattach(context.Background(), nets)
	mu.Lock()
	defer mu.Unlock()
	if len(joined) != 1 || !strings.HasSuffix(joined[0], "/networks/cloudex-cx-bbb222/connect cloudex-node "+nets["cx-bbb222"].Agent.String()) {
		t.Fatalf("joined %v", joined)
	}
}
