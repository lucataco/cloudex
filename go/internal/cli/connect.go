package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

func newExecCmd(opts *options) *cobra.Command {
	var root bool
	var cwd string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "exec [sandbox] -- <command>",
		Short: "Run a command in a sandbox and print its output",
		Long: "Run a command with bash in a sandbox, as the cloudex user in its home directory.\n" +
			"Without --, every argument is the command and the current sandbox runs it.",
		Example: "  cloudex exec -- npm test\n  cloudex exec my-box -- 'cd app && git status'",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			named, command := splitAtDash(cmd, args)
			if command == nil {
				named, command = nil, args
			}
			if len(named) > 1 {
				return errors.New("name at most one sandbox before --")
			}
			if len(command) == 0 {
				return errors.New("no command given after --")
			}
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, optionalRef(named))
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout+time.Minute)
			defer cancel()
			res, err := client.Exec(ctx, r, api.ExecRequest{
				Cmd: strings.Join(command, " "), Root: root, Cwd: cwd, TimeoutSec: int(timeout.Seconds()),
			})
			if err != nil {
				return err
			}
			if opts.json {
				if err := writeJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
			} else {
				fmt.Fprint(cmd.OutOrStdout(), res.Stdout)
				fmt.Fprint(cmd.ErrOrStderr(), res.Stderr)
				if res.TimedOut {
					fmt.Fprintf(cmd.ErrOrStderr(), "cloudex: stopped the command after %s\n", timeout)
				}
			}
			if res.ExitCode != 0 {
				return exitError{res.ExitCode}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&root, "root", false, "run as root")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (default: the user's home)")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "stop the command after this long (at most 1h)")
	return cmd
}

func newHostCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "host [sandbox] <port>",
		Short: "Get an HTTPS link to a port in a sandbox, on your preview domain",
		Long: "Print a link that serves a port in the sandbox over HTTPS on the domain you\n" +
			"chose at deploy time. The link contains a secret token, so only people you\n" +
			"share it with can open it.",
		Example: "  cloudex host 3000",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			named := ""
			if len(args) == 2 {
				named = args[0]
			}
			port, err := strconv.Atoi(args[len(args)-1])
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("bad port %q", args[len(args)-1])
			}
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, named)
			if err != nil {
				return err
			}
			url, err := client.Host(cmd.Context(), r, port)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"url": url})
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), url)
			return err
		},
	}
}

var agentKinds = map[string]bool{"claude": true, "codex": true}

func newPromptCmd(opts *options) *cobra.Command {
	var agent string
	var detach bool
	cmd := &cobra.Command{
		Use:   "prompt [sandbox] <task>",
		Short: "Give a coding agent a task in a sandbox and watch it work",
		Long: "Start Claude Code or Codex on a task in a tmux session inside the\n" +
			"sandbox, then attach to it. Detach with Ctrl-b d; the agent keeps working and\n" +
			"the sandbox stays up while it does. Reattach with cloudex attach.\n\n" +
			"The agent needs a key or a login: store an API key with cloudex keys set, or\n" +
			"sign in once (cloudex ssh, then run claude or codex), which is saved with the\n" +
			"sandbox's disk. Agents run without their own permission prompts; the sandbox\n" +
			"is the boundary.",
		Example: "  cloudex prompt \"make the tests pass\"\n  cloudex prompt my-box --agent codex \"add a dark mode\"",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !agentKinds[agent] {
				return fmt.Errorf("unknown agent %q: use claude or codex", agent)
			}
			named := ""
			if len(args) == 2 {
				named = args[0]
			}
			task := args[len(args)-1]
			start := fmt.Sprintf("cloudex-agent start %s %s", agent, shellQuote(task))
			remote := fmt.Sprintf(`name=$(%s) && echo "Started $name. Detach with Ctrl-b d; reattach with cloudex attach." && sleep 1 && exec cloudex-agent attach "$name"`, start)
			if detach {
				remote = fmt.Sprintf(`name=$(%s) && echo "Started $name. Watch it with: cloudex attach"`, start)
			}
			return sshRun(cmd, opts, named, !detach, remote)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "claude", "claude or codex")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "start the agent without attaching")
	return cmd
}

func newAttachCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "attach [sandbox] [session]",
		Short: "Reattach to an agent session (the newest by default)",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			named, session := "", ""
			if len(args) > 0 {
				named = args[0]
			}
			if len(args) > 1 {
				session = shellQuote(args[1])
			}
			return sshRun(cmd, opts, named, true, "cloudex-agent attach "+session)
		},
	}
}

// sshRun runs a remote command over SSH, with a terminal when tty is set.
func sshRun(cmd *cobra.Command, opts *options, named string, tty bool, remote string) error {
	cfg, client, err := opts.load()
	if err != nil {
		return err
	}
	r, err := ref(cfg, named)
	if err != nil {
		return err
	}
	s, err := client.Get(cmd.Context(), r)
	if err != nil {
		return err
	}
	if s.State != "running" {
		return fmt.Errorf("%s is %s; start it with: cloudex resume %s", s.Label(), s.State, s.Label())
	}
	args := []string{}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, "cloudex@"+s.ID+sshHostSuffix, remote)
	return runSSH(cmd, s.ID, args)
}
