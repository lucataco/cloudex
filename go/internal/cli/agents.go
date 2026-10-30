package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

var agentIDRE = regexp.MustCompile(`^ag-[a-z0-9]{8}$`)

// How long each request for an agent's events waits for new ones.
var followWaitSec = 30

func newAgentsCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agents [sandbox]",
		Short: "Run coding agents in a sandbox without a terminal, and follow them",
		Long: "Agents that work without a terminal, as the SDKs start them: Claude Code or\n" +
			"Codex takes a task, then any follow-up messages, one turn each. Their logs\n" +
			"live in the sandbox, so they survive stops, resumes and forks. With no\n" +
			"subcommand, lists a sandbox's agents.\n\n" +
			"Commands that take an agent use the most recently active one when it's left out.\n" +
			"For an agent in a terminal you can attach to, use cloudex prompt.",
		Example: "  cloudex agents start \"make the tests pass\"\n  cloudex agents logs -f\n" +
			"  cloudex agents send \"now add a test for the empty case\"\n  cloudex agents interrupt",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, optionalRef(args))
			if err != nil {
				return err
			}
			list, err := client.ListAgents(cmd.Context(), r)
			if err != nil {
				return err
			}
			sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
			if opts.json {
				if list == nil {
					list = []api.Agent{}
				}
				return writeJSON(cmd.OutOrStdout(), list)
			}
			if len(list) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "No agents in this sandbox yet. Start one with: cloudex agents start <task>")
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tAGENT\tSTATE\tTURNS\tUPDATED\tLAST MESSAGE")
			for _, a := range list {
				state := a.State
				if a.Queued > 0 {
					state += fmt.Sprintf(" (+%d queued)", a.Queued)
				}
				last := "-"
				if a.LastMessage != nil {
					last = oneLine(*a.LastMessage, 60)
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", a.ID, a.Agent, state, a.Turns, ago(a.UpdatedAt), last)
			}
			return tw.Flush()
		},
	}
	cmd.AddCommand(newAgentsStartCmd(opts), newAgentsLogsCmd(opts), newAgentsSendCmd(opts), newAgentsInterruptCmd(opts))
	return cmd
}

func newAgentsStartCmd(opts *options) *cobra.Command {
	var agent, model, cwd string
	var detach bool
	cmd := &cobra.Command{
		Use:   "start [sandbox] <task>",
		Short: "Start an agent on a task and follow it",
		Long: "Start Claude Code or Codex on a task, then print what it does until it\n" +
			"finishes. Ctrl-C stops following; the agent keeps working. A task of -\n" +
			"is read from standard input.\n\n" +
			"The agent needs a key or a login: store an API key with cloudex keys set, or\n" +
			"sign in once in the sandbox. Agents run without their own permission prompts;\n" +
			"the sandbox is the boundary.",
		Example: "  cloudex agents start \"make the tests pass\"\n" +
			"  cloudex agents start web --agent codex --cwd app \"add a dark mode\"\n  cloudex agents start -d - < task.md",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !agentKinds[agent] {
				return fmt.Errorf("unknown agent %q: use claude or codex", agent)
			}
			task, err := readPrompt(cmd, args[len(args)-1])
			if err != nil {
				return err
			}
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, optionalRef(args[:len(args)-1]))
			if err != nil {
				return err
			}
			a, err := client.StartAgent(cmd.Context(), r, api.StartAgentRequest{Prompt: task, Agent: agent, Cwd: cwd, Model: model})
			if err != nil {
				return err
			}
			if detach {
				if opts.json {
					return writeJSON(cmd.OutOrStdout(), a)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Started %s (%s). Follow it with: cloudex agents logs -f %s\n", a.ID, a.Agent, a.ID)
				return nil
			}
			if !opts.json {
				fmt.Fprintf(cmd.ErrOrStderr(), "Started %s (%s). Ctrl-C stops following; the agent keeps working.\n", a.ID, a.Agent)
			}
			return followAgent(cmd, opts, client, r, a, 0)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "claude", "claude or codex")
	cmd.Flags().StringVar(&model, "model", "", "the agent's model, such as opus or gpt-5 (default: the agent's own)")
	cmd.Flags().StringVar(&cwd, "cwd", "", "directory to work in (default: the home directory)")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "start the agent and print its id without following it")
	return cmd
}

func newAgentsLogsCmd(opts *options) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs [sandbox] [agent]",
		Short: "Print what an agent did",
		Long: "Print an agent's log: the prompts, the agent's messages, the commands it ran,\n" +
			"the files it changed, and how each turn ended. With --json, the events as\n" +
			"the API returns them, one per line, including the agent's own JSON.",
		Example: "  cloudex agents logs\n  cloudex agents logs -f ag-1a2b3c4d\n  cloudex agents logs web --json | jq .event",
		Args:    cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, r, a, err := pickAgent(cmd, opts, args)
			if err != nil {
				return err
			}
			if !follow {
				return printAgentEvents(cmd, opts, client, r, a, 0, false)
			}
			return followAgent(cmd, opts, client, r, a, 0)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing until the agent has nothing left to do")
	return cmd
}

func newAgentsSendCmd(opts *options) *cobra.Command {
	var interrupt, detach bool
	cmd := &cobra.Command{
		Use:   "send [sandbox] [agent] <message>",
		Short: "Send an agent a follow-up message",
		Long: "Send an agent its next message. It continues the same conversation, after\n" +
			"the running turn if there is one. --interrupt stops the running turn and drops\n" +
			"queued messages first, to steer the agent. Then prints what it does, like\n" +
			"start. A message of - is read from standard input.",
		Example: "  cloudex agents send \"now add a test for the empty case\"\n" +
			"  cloudex agents send --interrupt ag-1a2b3c4d \"stop, use the existing helper instead\"",
		Args: cobra.RangeArgs(1, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			message, err := readPrompt(cmd, args[len(args)-1])
			if err != nil {
				return err
			}
			client, r, a, err := pickAgent(cmd, opts, args[:len(args)-1])
			if err != nil {
				return err
			}
			from := a.Events
			if a, err = client.SendAgent(cmd.Context(), r, a.ID, message, interrupt); err != nil {
				return err
			}
			if detach {
				if opts.json {
					return writeJSON(cmd.OutOrStdout(), a)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Sent to %s (%s).\n", a.ID, queueState(a))
				return nil
			}
			return followAgent(cmd, opts, client, r, a, from)
		},
	}
	cmd.Flags().BoolVar(&interrupt, "interrupt", false, "stop the running turn and drop queued messages first")
	cmd.Flags().BoolVarP(&detach, "detach", "d", false, "send without following the agent")
	return cmd
}

func newAgentsInterruptCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:     "interrupt [sandbox] [agent]",
		Aliases: []string{"stop"},
		Short:   "Stop an agent's running turn and drop its queued messages",
		Long: "Stop the turn an agent is working on and drop any queued messages. The\n" +
			"conversation stays: send a message to carry on.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, r, a, err := pickAgent(cmd, opts, args)
			if err != nil {
				return err
			}
			if a, err = client.InterruptAgent(cmd.Context(), r, a.ID); err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), a)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Interrupted %s; it's %s.\n", a.ID, a.State)
			return err
		},
	}
}

// pickAgent reads [sandbox] [agent] arguments: an argument that looks like an
// agent id is the agent, and without one, the sandbox's most recently active
// agent is used.
func pickAgent(cmd *cobra.Command, opts *options, args []string) (*api.Client, string, *api.Agent, error) {
	named, id := "", ""
	switch {
	case len(args) == 2:
		named, id = args[0], args[1]
	case len(args) == 1 && agentIDRE.MatchString(args[0]):
		id = args[0]
	case len(args) == 1:
		named = args[0]
	}
	if id != "" && !agentIDRE.MatchString(id) {
		return nil, "", nil, fmt.Errorf("%q isn't an agent id, such as ag-1a2b3c4d", id)
	}
	cfg, client, err := opts.load()
	if err != nil {
		return nil, "", nil, err
	}
	r, err := ref(cfg, named)
	if err != nil {
		return nil, "", nil, err
	}
	if id != "" {
		a, err := client.GetAgent(cmd.Context(), r, id)
		return client, r, a, err
	}
	list, err := client.ListAgents(cmd.Context(), r)
	if err != nil {
		return nil, "", nil, err
	}
	if len(list) == 0 {
		return nil, "", nil, fmt.Errorf("%s has no agents yet; start one with cloudex agents start <task>", r)
	}
	newest := list[0]
	for _, a := range list[1:] {
		if a.UpdatedAt > newest.UpdatedAt {
			newest = a
		}
	}
	return client, r, &newest, nil
}

// followAgent prints the agent's events after seq from until it has nothing
// left to do, then ends with the last turn's exit code. Ctrl-C stops
// following without stopping the agent.
func followAgent(cmd *cobra.Command, opts *options, client *api.Client, r string, a *api.Agent, from int) error {
	ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cmd.SetContext(ctx)
	err := printAgentEvents(cmd, opts, client, r, a, from, true)
	if ctx.Err() != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "\nStopped following %s; it keeps working. Follow it again with: cloudex agents logs -f %s\n", a.ID, a.ID)
		return nil
	}
	return err
}

// printAgentEvents prints events after seq after, page by page; with follow,
// it waits for new ones until the agent is idle.
func printAgentEvents(cmd *cobra.Command, opts *options, client *api.Client, r string, a *api.Agent, after int, follow bool) error {
	p := newAgentPrinter(cmd.OutOrStdout(), a.Agent)
	for {
		wait := 0
		if follow {
			wait = followWaitSec
		}
		ev, err := client.AgentEvents(cmd.Context(), r, a.ID, after, wait)
		if err != nil {
			if errors.Is(cmd.Context().Err(), context.Canceled) {
				return nil
			}
			return err
		}
		for _, e := range ev.Events {
			if opts.json {
				err = writeJSONLine(cmd.OutOrStdout(), e)
			} else {
				err = p.print(e)
			}
			if err != nil {
				return err
			}
		}
		after = ev.Next
		*a = ev.Agent
		if after >= a.Events && (!follow || a.State == "idle") {
			break
		}
	}
	if follow && a.LastExitCode != nil && *a.LastExitCode != 0 {
		return exitError{*a.LastExitCode}
	}
	return nil
}

// readPrompt returns arg, or standard input when arg is -.
func readPrompt(cmd *cobra.Command, arg string) (string, error) {
	if arg != "-" {
		return arg, nil
	}
	data, err := io.ReadAll(cmd.InOrStdin())
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", errors.New("standard input was empty")
	}
	return string(data), nil
}

func queueState(a *api.Agent) string {
	if a.Queued > 0 {
		return fmt.Sprintf("%s, %d queued", a.State, a.Queued)
	}
	return a.State
}

// oneLine is the first line of s, cut to n characters.
func oneLine(s string, n int) string {
	s, _, cut := strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	} else if cut {
		return s + " …"
	}
	return s
}
