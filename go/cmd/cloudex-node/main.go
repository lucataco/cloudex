// Command cloudex-node is the agent on a node: a user's own host that runs
// sandboxes for a Cloudex deployment. It dials out to the deployment and keeps
// a control socket open; the host needs no inbound port.
//
//	cloudex-node join --api https://<deployment> --token cxj_...
//	cloudex-node run [--join cxj_... --api https://<deployment>]
//	cloudex-node status
//
// It keeps its key and identity in a state directory: /var/lib/cloudex-node
// (a Docker volume when it runs as the cloudex-node container), or wherever
// --state-dir or CLOUDEX_NODE_STATE say, as a plain process.
package main

import (
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
			id, err := doJoin(cmd.Context(), state(), apiURL, token, unisolated)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Joined as %s (%s). Start the agent with: cloudex-node run\n", id.Name, id.ID)
			return nil
		},
	}
	join.Flags().StringVar(&apiURL, "api", "", "the deployment's URL")
	join.Flags().StringVar(&token, "token", "", "the join token (cxj_...)")
	join.Flags().BoolVar(&unisolated, "unisolated", false, "for testing only: run machines without the firewall's isolation")
	join.MarkFlagRequired("api")
	join.MarkFlagRequired("token")

	var runJoin, runAPI string
	var runUnisolated bool
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
			a := &node.Agent{API: id.APIURL, ID: id.ID, Key: key, Version: version, Unisolated: id.Unisolated, State: s, Log: lg}
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

	var asJSON bool
	status := &cobra.Command{
		Use:   "status",
		Short: "Say whether the agent is connected; exits 1 when it isn't",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := state()
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

	root.AddCommand(join, run, status, newProbe(state))
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
	probe.AddCommand(connect, join)
	return probe
}
