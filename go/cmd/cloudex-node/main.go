// Command cloudex-node is the agent on a node: a user's own host that runs
// sandboxes for a Cloudex deployment. It dials out to the deployment and keeps
// a control socket open; the host needs no inbound port.
//
//	cloudex-node join --api https://<deployment> --token - < token-file
//	cloudex-node run [--join cxj_... --api https://<deployment>]
//	cloudex-node status
//
// It keeps its key and identity in a state directory: /var/lib/cloudex-node
// (a Docker volume when it runs as the cloudex-node container), or wherever
// --state-dir or CLOUDEX_NODE_STATE say, as a plain process.
package main

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/node"
	"github.com/spf13/cobra"
)

// version is set at release time with -ldflags "-X main.version=v0.2.2".
var version = "dev"

func main() {
	if err := newRoot().Execute(); err != nil {
		var exit exitCode
		if errors.As(err, &exit) {
			os.Exit(int(exit))
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func newRoot() *cobra.Command {
	var stateDir string
	root := &cobra.Command{
		Use:           "cloudex-node",
		Short:         "The agent that lets this host run Cloudex sandboxes",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&stateDir, "state-dir", "", "where the node's key and identity live (default $CLOUDEX_NODE_STATE, else "+node.DefaultStateDir+")")
	state := func() node.State { return node.State{Dir: node.StateDir(stateDir)} }

	var apiURL, token string
	var unisolated bool
	join := &cobra.Command{
		Use:   "join",
		Short: "Join a deployment with the token cloudex node add printed",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if token == "-" {
				// On stdin, so that it isn't in the process's arguments (review H9).
				line, err := bufio.NewReader(io.LimitReader(cmd.InOrStdin(), 4096)).ReadString('\n')
				if err != nil && line == "" {
					return fmt.Errorf("reading the join token from stdin: %w", err)
				}
				token = strings.TrimSpace(line)
			}
			id, err := doJoin(cmd.Context(), state(), apiURL, token, unisolated)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Joined as %s (%s). Start the agent with: cloudex-node run\n", id.Name, id.ID)
			return nil
		},
	}
	join.Flags().StringVar(&apiURL, "api", "", "the deployment's URL")
	join.Flags().StringVar(&token, "token", "", "the join token (cxj_...), or - to read it from stdin")
	join.Flags().BoolVar(&unisolated, "unisolated", false, "for testing only: run machines without the firewall's isolation")
	join.MarkFlagRequired("api")
	join.MarkFlagRequired("token")

	var runJoin, runAPI, dockerSocket string
	var runUnisolated bool
	var spares int
	var rc node.RuntimeConfig
	run := &cobra.Command{
		Use:   "run",
		Short: "Keep this node connected to its deployment (joining first with --join)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			lg := log.New(os.Stderr, "cloudex-node ", log.LstdFlags|log.Lmsgprefix)
			s := state()
			id, err := s.Identity()
			if err != nil {
				return err
			}
			switch {
			case id == nil && runJoin == "":
				return fmt.Errorf("this node hasn't joined a deployment (no %s/node.json): run with --join <token> --api <url>, or cloudex-node join first", s.Dir)
			case id == nil:
				if id, err = doJoin(cmd.Context(), s, runAPI, runJoin, runUnisolated); err != nil {
					return err
				}
				lg.Printf("joined as %s (%s)", id.Name, id.ID)
			case runJoin != "":
				lg.Printf("already joined as %s (%s); ignoring --join", id.Name, id.ID)
			}
			key, err := s.Key()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			lg.Printf("%s: node %s (%s) connecting to %s", version, id.Name, id.ID, id.APIURL)
			d := &node.Docker{Socket: dockerSocket}
			a := &node.Agent{API: id.APIURL, ID: id.ID, Key: key, Version: version, Unisolated: id.Unisolated, State: s, Log: lg,
				Docker: d, Spares: spares, Debug: os.Getenv("CLOUDEX_NODE_DEBUG") == "1"}
			if rc.Pool != "" {
				rc.Self = selfContainer(rc.Self)
				if err := s.SaveRuntimeConfig(rc); err != nil {
					return err
				}
				r, err := rc.Runtime(d, lg)
				if err != nil {
					return err
				}
				a.Runtime = r
				lg.Printf("machines: pool %s, agent container %q, images %v%s", rc.Pool, rc.Self, r.Allowed, map[bool]string{true: ", unisolated (testing only)"}[id.Unisolated])
			} else {
				lg.Printf("no --pool: this agent runs no machines")
			}
			if err := a.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			lg.Printf("stopped")
			return nil
		},
	}
	run.Flags().StringVar(&runJoin, "join", "", "a join token, used when this node hasn't joined yet")
	run.Flags().StringVar(&runAPI, "api", "", "the deployment's URL, with --join")
	run.Flags().BoolVar(&runUnisolated, "unisolated", false, "with --join: for testing only, run machines without the firewall's isolation")
	run.Flags().IntVar(&spares, "spares", node.SparesMin, "spare stream sockets kept open per machine")
	run.Flags().StringVar(&dockerSocket, "docker-socket", node.DefaultDockerSocket, "the Docker Engine API's socket")
	run.Flags().StringVar(&rc.Pool, "pool", "", "the machine pool, such as 10.250.0.0/16 (the installer picks one); without it, no machines")
	run.Flags().StringVar(&rc.Self, "container", "", "the agent's own container (default $CLOUDEX_NODE_CONTAINER, else its hostname inside a container)")
	run.Flags().StringArrayVar(&rc.Allowed, "allow-image", nil, "an image repository machines may run, besides "+node.DefaultImageRepo+"; repeatable")
	run.Flags().StringVar(&rc.Image, "machine-image", "", "run machines from this image instead of the deployment's (it must be allowed)")
	run.Flags().StringVar(&rc.DockerRoot, "docker-root", "", "where Docker's data root is mounted read-only, for its free space")
	run.Flags().StringArrayVar(&rc.Probes, "probe", nil, "an address of the host or its LAN (ip:port, or udp53=ip) the self-test must find closed; repeatable")
	run.Flags().Float64Var(&rc.KeepFreeGB, "keep-free-gb", node.KeepFreeGB, "below this much free space on Docker's disk, stop the machine that wrote the most; new machines need 5 GB more")
	run.Flags().IntVar(&rc.DiskGB, "disk-gb", 0, "cap each machine's writable layer at this size, where Docker's storage driver can (btrfs, zfs, overlay2 on xfs with pquota); 0: no cap")

	var asJSON bool
	var wait time.Duration
	var since int64
	status := &cobra.Command{
		Use:   "status",
		Short: "Say whether the agent is connected; exits 1 when it isn't",
		Long: "Say whether the agent is connected; exits 1 when it isn't. With --wait, waits up to\n" +
			"that long for it to be connected and to have run its isolation self-test, then\n" +
			"exits 0 if the self-test passed and 2 if it failed (the installer's step 5). With\n" +
			"--since, only a self-test from that time on counts (unix ms): the status file may\n" +
			"still hold the result of an agent this one replaced.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := state()
			if wait > 0 {
				return waitReady(cmd, s, wait, since)
			}
			id, err := s.Identity()
			if err != nil {
				return err
			}
			st, _ := s.ReadStatus()
			running := st != nil && alive(st.PID)
			up := running && st.Connected && time.Since(time.UnixMilli(st.LastPongAt)) < node.PongDead
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				enc.Encode(map[string]any{"joined": id != nil, "identity": id, "connected": up, "status": st})
			} else {
				switch {
				case id == nil:
					fmt.Fprintf(out, "Not joined (no %s/node.json).\n", s.Dir)
				case up:
					fmt.Fprintf(out, "%s (%s) is connected to %s since %s.\n", id.Name, id.ID, id.APIURL, time.UnixMilli(st.Since).Format(time.RFC3339))
				case st != nil && st.Revoked:
					fmt.Fprintf(out, "%s (%s) was REMOVED from %s (cloudex node rm): its machines were stopped, and their disks stay here until you uninstall the agent (node-install.sh --uninstall).\n", id.Name, id.ID, id.APIURL)
				case st == nil:
					fmt.Fprintf(out, "%s (%s) has joined %s, but the agent hasn't run yet.\n", id.Name, id.ID, id.APIURL)
				case !running:
					fmt.Fprintf(out, "%s (%s) isn't connected to %s: the agent (pid %d) isn't running.\n", id.Name, id.ID, id.APIURL, st.PID)
				default:
					fmt.Fprintf(out, "%s (%s) isn't connected to %s. Last: %s\n", id.Name, id.ID, id.APIURL, firstNonEmpty(st.LastError, "the agent stopped"))
				}
			}
			if !up {
				return exitCode(1)
			}
			return nil
		},
	}
	status.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	status.Flags().DurationVar(&wait, "wait", 0, "wait this long for the connection and the self-test's result")
	status.Flags().Int64Var(&since, "since", 0, "with --wait, count only a self-test from this time on (unix ms)")

	root.AddCommand(join, run, status, newProbe(state), newFirewall(), newPickPool(), newSelfTest(state, &dockerSocket), newCA(state))
	return root
}

func doJoin(ctx context.Context, s node.State, apiURL, token string, unisolated bool) (*node.Identity, error) {
	if old, err := s.Identity(); err != nil {
		return nil, err
	} else if old != nil {
		return nil, fmt.Errorf("this node has joined already, as %s (%s) of %s; to join again, remove %s first", old.Name, old.ID, old.APIURL, s.Dir)
	}
	apiURL = api.NormalizeURL(apiURL)
	key, err := s.Key()
	if err != nil {
		return nil, err
	}
	j, err := node.JoinNode(ctx, nil, apiURL, token, version, key, node.HostFacts(unisolated))
	if err != nil {
		return nil, fmt.Errorf("joining: %w", err)
	}
	id := node.Identity{ID: j.Node.ID, Name: j.Node.Name, APIURL: apiURL, Unisolated: unisolated, JoinedAt: time.Now().UnixMilli()}
	return &id, s.SaveIdentity(id)
}

// waitReady is status --wait.
func waitReady(cmd *cobra.Command, s node.State, wait time.Duration, since int64) error {
	out := cmd.OutOrStdout()
	deadline := time.Now().Add(wait)
	said := ""
	for {
		id, _ := s.Identity()
		st, _ := s.ReadStatus()
		up := id != nil && st != nil && alive(st.PID) && st.Connected && time.Since(time.UnixMilli(st.LastPongAt)) < node.PongDead
		iso := (*node.Isolation)(nil)
		if st != nil && st.Isolation != nil && st.Isolation.CheckedAt >= since {
			iso = st.Isolation
		}
		switch {
		case up && iso != nil && iso.Firewall == "pass":
			fmt.Fprintf(out, "%s is online, and its isolation self-test passed\n", id.Name)
			return nil
		case up && iso != nil && iso.Firewall == "fail":
			fmt.Fprintf(out, "%s is online, but its isolation self-test failed:\n", id.Name)
			for _, f := range iso.Failed {
				fmt.Fprintf(out, "  %s\n", f)
			}
			return exitCode(2)
		}
		now := "waiting for the agent to connect"
		if up {
			now = "connected; waiting for the isolation self-test"
		} else if st != nil && st.LastError != "" {
			now = "not connected yet: " + st.LastError
		}
		if now != said {
			fmt.Fprintln(out, now)
			said = now
		}
		if time.Now().After(deadline) {
			return exitCode(1)
		}
		time.Sleep(time.Second)
	}
}

// selfContainer is the agent's own container: the flag, else
// $CLOUDEX_NODE_CONTAINER, else its hostname when it runs in a container.
func selfContainer(flag string) string {
	if flag != "" {
		return flag
	}
	if env := os.Getenv("CLOUDEX_NODE_CONTAINER"); env != "" {
		return env
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		h, _ := os.Hostname()
		return h
	}
	return ""
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// newProbe is for the release checks (verify.sh, area node): requests with a
// fault in them, which the deployment must refuse. Each prints the HTTP status
// it got. It uses this node's own key, so it runs on the node.
func newProbe(state func() node.State) *cobra.Command {
	probe := &cobra.Command{Use: "probe", Hidden: true, Short: "Send requests the deployment must refuse (release checks)"}
	var fault string
	connect := &cobra.Command{
		Use:   "connect",
		Short: "Open a control socket with --fault none, badsig, skew, replay or badmsg, and print the status (badmsg: the close code)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := state()
			id, err := s.Identity()
			if err != nil || id == nil {
				return fmt.Errorf("not joined: %v", err)
			}
			key, err := s.Key()
			if err != nil {
				return err
			}
			nonce := ""
			try := func(f string) (int, string) {
				full, signed, _ := node.Endpoint(id.APIURL, "/v1/node/connect")
				h := http.Header{}
				h.Set(node.HeaderProtocol, strconv.Itoa(node.Protocol))
				sg := node.Signer{Key: key, Node: id.ID}
				switch f {
				case "skew":
					sg.Now = func() time.Time { return time.Now().Add(-2 * time.Minute) }
				case "replay":
					sg.Nonce = func() string { return nonce }
				}
				sg.Sign(h, http.MethodGet, signed, "")
				nonce = h.Get(node.HeaderNonce)
				if f == "badsig" {
					sig, _ := base64.StdEncoding.DecodeString(h.Get(node.HeaderSig))
					sig[0] ^= 1
					h.Set(node.HeaderSig, base64.StdEncoding.EncodeToString(sig))
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
				defer cancel()
				c, res, err := websocket.Dial(ctx, full, &websocket.DialOptions{HTTPHeader: h})
				if c != nil && f == "badmsg" {
					// A message outside the protocol: the answer is the close code.
					c.Write(ctx, websocket.MessageText, []byte(`{"type":"exec","argv":["sh"]}`))
					_, _, err := c.Read(ctx)
					return int(websocket.CloseStatus(err)), fmt.Sprint(err)
				}
				if c != nil {
					c.Close(websocket.StatusNormalClosure, "probe")
				}
				if res == nil {
					return 0, fmt.Sprint(err)
				}
				body := ""
				if res.Body != nil {
					b, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
					body = string(b)
				}
				return res.StatusCode, body
			}
			if fault == "replay" {
				// The first use of the nonce is accepted, and replaces the
				// node's own connection: run it while the agent is stopped.
				if code, body := try("none"); code != 101 {
					return fmt.Errorf("the first connect, before the replay, got %d %s", code, body)
				}
			}
			code, body := try(fault)
			fmt.Fprintln(cmd.OutOrStdout(), code)
			fmt.Fprintln(cmd.ErrOrStderr(), body)
			return nil
		},
	}
	connect.Flags().StringVar(&fault, "fault", "none", "none, badsig, skew, replay or badmsg")

	var sandbox string
	stream := &cobra.Command{
		Use:   "stream",
		Short: "Get a stream ticket for --sandbox over the control socket, then open a stream with it, again (a replay), with it altered, and for another stream id; print the four statuses",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := state()
			id, err := s.Identity()
			if err != nil || id == nil {
				return fmt.Errorf("not joined: %v", err)
			}
			key, err := s.Key()
			if err != nil {
				return err
			}
			codes, err := node.ProbeStreams(cmd.Context(), id.APIURL, id.ID, key, version, sandbox)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), strings.Trim(fmt.Sprint(codes), "[]"))
			return nil
		},
	}
	stream.Flags().StringVar(&sandbox, "sandbox", "", "a sandbox on this node")

	var apiURL, token string
	join := &cobra.Command{
		Use:   "join",
		Short: "Try a join token with a throwaway key, and print the status",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, key, _ := ed25519.GenerateKey(nil)
			_, err := node.JoinNode(cmd.Context(), nil, api.NormalizeURL(apiURL), token, version, key, node.HostFacts(false))
			var refused *node.RefusedError
			switch {
			case err == nil:
				fmt.Fprintln(cmd.OutOrStdout(), 200)
			case errors.As(err, &refused):
				fmt.Fprintln(cmd.OutOrStdout(), refused.Status)
				fmt.Fprintln(cmd.ErrOrStderr(), refused.Message)
			default:
				return err
			}
			return nil
		},
	}
	join.Flags().StringVar(&apiURL, "api", "", "the deployment's URL")
	join.Flags().StringVar(&token, "token", "", "the join token")
	// 2026-11-12 review, W1 and W2: misbehaving agents (probe_flood.go).
	var other string
	var count int
	flood := &cobra.Command{
		Use:   "flood",
		Short: "Connect as the node and flood the control socket: facts, state and ticket requests for --other (a sandbox not on this node), --count of each, then a second hello; print what the deployment did, as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := state()
			id, err := s.Identity()
			if err != nil || id == nil {
				return fmt.Errorf("not joined: %v", err)
			}
			key, err := s.Key()
			if err != nil {
				return err
			}
			rep, err := node.ProbeFlood(cmd.Context(), id.APIURL, id.ID, key, version, other, count)
			if err != nil {
				return err
			}
			b, _ := json.Marshal(rep)
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return nil
		},
	}
	flood.Flags().StringVar(&other, "other", "", "a sandbox id that isn't on this node")
	flood.Flags().IntVar(&count, "count", 500, "how many of each message")

	var splice bool
	var runFor time.Duration
	var spliceMax, overrunMax int64
	fake := &cobra.Command{
		Use:   "fake-agent",
		Short: "Run as the node's agent without machines, answering create and start, with execs of cx-flood-overrun and cx-flood-channel (and with --splice, ssh) flooding their streams; print one JSON line per flood",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := state()
			id, err := s.Identity()
			if err != nil || id == nil {
				return fmt.Errorf("not joined: %v", err)
			}
			key, err := s.Key()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), runFor)
			defer cancel()
			var mu sync.Mutex
			out := cmd.OutOrStdout()
			f := &node.FakeAgent{API: id.APIURL, NodeID: id.ID, Version: version, Key: key, Splice: splice, SpliceMax: spliceMax, OverrunMax: overrunMax,
				Report: func(m map[string]any) {
					b, _ := json.Marshal(m)
					mu.Lock()
					fmt.Fprintln(out, string(b))
					mu.Unlock()
				},
				Log: func(format string, args ...any) { fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...) },
			}
			// Again after a drop (an edge reset ends a control socket now and
			// then), as the real agent does, until --for is up.
			for ctx.Err() == nil {
				err := f.Run(ctx)
				if ctx.Err() != nil {
					break
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "connection ended (%v); connecting again\n", err)
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
			return nil
		},
	}
	fake.Flags().BoolVar(&splice, "splice", false, "flood ssh splices too")
	fake.Flags().DurationVar(&runFor, "for", 10*time.Minute, "how long to run")
	fake.Flags().Int64Var(&spliceMax, "splice-max", 256<<20, "the most a splice flood sends")
	fake.Flags().Int64Var(&overrunMax, "overrun-max", 96<<20, "the most an overrun flood sends")
	probe.AddCommand(connect, join, stream, flood, fake)
	return probe
}
