package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

func newExecCmd(opts *options) *cobra.Command {
	var root, detach bool
	var cwd string
	timeout := 10 * time.Minute
	cmd := &cobra.Command{
		Use:   "exec [sandbox] -- <command>",
		Short: "Run a command in a sandbox, or start one in the background",
		Long: "Run a command with bash in a sandbox, as the cloudex user in its home directory,\n" +
			"printing its output as it comes and ending with its exit code. One argument is\n" +
			"a shell command line; several are one command and its arguments, quoted for you.\n" +
			"Without --, every argument is the command and the current sandbox runs it.\n\n" +
			"--timeout (at most 1h) stops the command and everything it started. Ctrl-C stops\n" +
			"it too. For longer jobs, and servers, use --detach: the command keeps running in\n" +
			"the sandbox, and cloudex commands shows it and its output.\n\n" +
			"Standard input isn't sent to the command; it sees none. To send data in, use\n" +
			"cloudex cp (a <from> of - reads standard input), or ssh with a redirect:\n" +
			"cloudex ssh <sandbox> -- 'cat > file' < file. Output is text, so for a binary\n" +
			"file use cloudex cp <sandbox>:file - instead.\n\n" +
			"With --json, prints the result as JSON once the command ends, output cut at\n" +
			"1 MiB per stream; with --detach, the background command.",
		Example: "  cloudex exec -- npm test\n" +
			"  cloudex exec my-box -- 'cd app && git status'\n" +
			"  cloudex exec -- python3 -c 'print(42)'\n" +
			"  cloudex exec -d -- npm run dev",
		Args: cobra.MinimumNArgs(1),
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
			line := commandLine(command)
			if detach {
				secs := 0
				if cmd.Flags().Changed("timeout") {
					if timeout < time.Second {
						return errors.New("--timeout must be at least 1s")
					}
					secs = int(timeout.Seconds())
				}
				c, err := client.StartCommand(cmd.Context(), r, api.StartCommandRequest{Cmd: line, Root: root, Cwd: cwd, TimeoutSec: secs})
				if err != nil {
					return err
				}
				if opts.json {
					return writeJSON(cmd.OutOrStdout(), c)
				}
				where := c.ID
				if len(named) > 0 {
					where = named[0] + " " + c.ID
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Started %s in the background in %s.\n", c.ID, r)
				fmt.Fprintf(cmd.OutOrStdout(), "Follow its output with: cloudex commands logs -f %s\nStop it with: cloudex commands kill %s\n", where, where)
				return nil
			}
			if timeout < time.Second || timeout > time.Hour {
				return errors.New("--timeout must be from 1s to 1h; for longer, use --detach")
			}
			req := api.ExecRequest{Cmd: line, Root: root, Cwd: cwd, TimeoutSec: int(timeout.Seconds())}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout+2*time.Minute)
			defer cancel()
			if opts.json && cmd.Flag("json").Changed {
				res, err := client.Exec(ctx, r, req)
				if err != nil {
					return err
				}
				if err := writeJSON(cmd.OutOrStdout(), res); err != nil {
					return err
				}
				if res.ExitCode != 0 {
					return exitError{res.ExitCode}
				}
				return nil
			}
			// Ctrl-C ends the stream, which stops the command.
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			end, err := client.ExecStream(ctx, r, req, func(ev api.ExecEvent) error {
				w := out
				if ev.Type == "stderr" {
					w = errOut
				}
				_, err := io.WriteString(w, ev.Data)
				return err
			})
			if err != nil {
				if errors.Is(err, context.Canceled) && cmd.Context().Err() == nil {
					fmt.Fprintln(errOut, "\ncloudex: stopped the command")
					return exitError{130}
				}
				return err
			}
			if end.TimedOut {
				fmt.Fprintf(errOut, "cloudex: stopped the command after %s\n", timeout)
			}
			if end.ExitCode != 0 {
				return exitError{end.ExitCode}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&root, "root", false, "run as root")
	cmd.Flags().StringVar(&cwd, "cwd", "", "working directory (default: the user's home)")
	cmd.Flags().Var(durationFlag{&timeout}, "timeout", "stop the command after this long, such as 30m (at most 1h; with --detach, any, and none by default)")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "start the command in the background and print its id")
	return cmd
}

// commandLine makes exec's arguments one bash command line: a single argument
// as it is, several quoted so that each stays one word.
func commandLine(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	words := make([]string, len(args))
	for i, a := range args {
		words[i] = shellWord(a)
	}
	return strings.Join(words, " ")
}

var plainWord = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellWord quotes s for a POSIX shell when it needs it.
func shellWord(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return shellQuote(s)
}

// isNumber is for telling a port from a sandbox, whose name starts with a letter.
func isNumber(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

func newHostCmd(opts *options) *cobra.Command {
	var list, revoke, revokeAll bool
	cmd := &cobra.Command{
		Use:   "host [sandbox] <port>",
		Short: "Get an HTTPS link to a port in a sandbox, on your preview domain",
		Long: "Print a link that serves a port in the sandbox over HTTPS on the domain you\n" +
			"chose at deploy time. The link contains a secret token, so only people you\n" +
			"share it with can open it. Servers that listen only on localhost work too.\n\n" +
			"The same port gives the same link until you revoke it with --revoke, which\n" +
			"makes it stop working at once and closes connections open through it; run\n" +
			"cloudex host again for a new one. --revoke-all does that for every link to the\n" +
			"sandbox, the desktop's too, including links made before v0.1.14, which --list\n" +
			"doesn't show.",
		Example: "  cloudex host 3000\n  cloudex host my-box 5173\n  cloudex host 3000 --revoke\n  cloudex host --list",
		Args:    cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			if list || revokeAll {
				if revoke || (list && revokeAll) {
					return errors.New("use one of --list, --revoke and --revoke-all")
				}
				if len(args) > 1 || len(args) == 1 && isNumber(args[0]) {
					return errors.New("--list and --revoke-all take a sandbox, not a port")
				}
				cfg, client, err := opts.load()
				if err != nil {
					return err
				}
				r, err := ref(cfg, optionalRef(args))
				if err != nil {
					return err
				}
				if revokeAll {
					res, err := client.RevokeLinks(cmd.Context(), r, 0)
					if err != nil {
						return err
					}
					if opts.json {
						return writeJSON(out, res)
					}
					_, err = fmt.Fprintf(out, "Every link to %s has stopped working, its desktop's too. Run cloudex host for new ones.\n", r)
					return err
				}
				links, err := client.Links(cmd.Context(), r)
				if err != nil {
					return err
				}
				if opts.json {
					return writeJSON(out, links)
				}
				if len(links) == 0 {
					_, err = fmt.Fprintln(out, "No links made since Cloudex v0.1.14. Make one with: cloudex host <port>")
					return err
				}
				tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "PORT\tLINK\tMADE")
				for _, l := range links {
					fmt.Fprintf(tw, "%d\t%s\t%s\n", l.Port, l.URL, ago(l.CreatedAt))
				}
				return tw.Flush()
			}
			if len(args) == 0 {
				return errors.New("give a port, such as: cloudex host 3000")
			}
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
			if revoke {
				res, err := client.RevokeLinks(cmd.Context(), r, port)
				if err != nil {
					return err
				}
				if opts.json {
					return writeJSON(out, res)
				}
				_, err = fmt.Fprintf(out, "The link to port %d on %s has stopped working. Run cloudex host %s %d for a new one.\n", port, r, r, port)
				return err
			}
			url, err := client.Host(cmd.Context(), r, port)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(out, map[string]string{"url": url})
			}
			_, err = fmt.Fprintln(out, url)
			return err
		},
	}
	cmd.Flags().BoolVar(&revoke, "revoke", false, "make the port's link stop working")
	cmd.Flags().BoolVar(&revokeAll, "revoke-all", false, "make every link to the sandbox stop working, the desktop's too")
	cmd.Flags().BoolVar(&list, "list", false, "list the links made since v0.1.14 that still work")
	return cmd
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
		Args:    taskArgs(2, "task"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !agentKinds[agent] {
				return fmt.Errorf("unknown agent %q: use claude or codex", agent)
			}
			named := ""
			if len(args) == 2 {
				named = args[0]
			}
			task := args[len(args)-1]
			// The hint goes inside double quotes in the remote shell, so only a name that is safe there is put in it.
			attach := "cloudex attach"
			if sandboxRefRE.MatchString(named) {
				attach += " " + named
			}
			start := fmt.Sprintf("cloudex-agent start %s %s", agent, shellQuote(task))
			remote := fmt.Sprintf(`name=$(%s) && echo "Started $name. Detach with Ctrl-b d; reattach with %s." && sleep 1 && exec cloudex-agent attach "$name"`, start, attach)
			if detach {
				remote = fmt.Sprintf(`name=$(%s) && echo "Started $name. Watch it with: %s"`, start, attach)
			}
			return unquoted(sshRun(cmd, opts, named, !detach, remote), args)
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
		Long: "Reattach to a session that cloudex prompt started, the newest by default. A\n" +
			"session is named like agent-1104-153012; cloudex prompt prints its name.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			named, session := attachTarget(args)
			if session != "" {
				session = shellQuote(session)
			}
			return sshRun(cmd, opts, named, true, "cloudex-agent attach "+session)
		},
	}
}

var sessionRE = regexp.MustCompile(`^agent-\d{4}-\d{6}(-\d+)?$`)

// attachTarget reads [sandbox] [session]. A lone argument that looks like a
// session's name is one; sandboxes can be named agent-1 and so on, but not like that.
func attachTarget(args []string) (named, session string) {
	switch {
	case len(args) == 2:
		return args[0], args[1]
	case len(args) == 1 && sessionRE.MatchString(args[0]):
		return "", args[0]
	case len(args) == 1:
		return args[0], ""
	}
	return "", ""
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
	if err := needRunning(s); err != nil {
		return err
	}
	args := []string{}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, "cloudex@"+s.ID+sshHostSuffix, remote)
	return runSSH(cmd, s.ID, args)
}
