package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

// How long each read of a followed command's output waits for more.
const commandWaitSec = 30

var (
	commandRetryFor   = 2 * time.Minute
	commandRetryEvery = 3 * time.Second
)

func newCommandsCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "commands [sandbox]",
		Short: "List background commands, follow their output, or stop them",
		Long: "List the commands started in the background with cloudex exec --detach (or the\n" +
			"API), newest first. Their output stays on the sandbox's disk until you remove\n" +
			"them with cloudex commands rm. A stop ends them; they show as lost after.",
		Example: "  cloudex commands\n  cloudex commands logs -f cm-1a2b3c4d\n  cloudex commands kill cm-1a2b3c4d",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, optionalRef(args))
			if err != nil {
				return err
			}
			list, err := client.ListCommands(cmd.Context(), r)
			if err != nil {
				return err
			}
			if opts.json {
				if list == nil {
					list = []api.Command{}
				}
				return writeJSON(cmd.OutOrStdout(), list)
			}
			printCommands(cmd.OutOrStdout(), list)
			return nil
		},
	}
	cmd.AddCommand(newCommandsLogsCmd(opts), newCommandsKillCmd(opts), newCommandsRmCmd(opts))
	return cmd
}

func printCommands(w io.Writer, list []api.Command) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No background commands. Start one with: cloudex exec -d -- <command>")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tSTARTED\tTOOK\tOUTPUT\tCOMMAND")
	for _, c := range list {
		took := "-"
		if c.EndedAt != nil {
			took = (time.Duration(*c.EndedAt-c.StartedAt) * time.Millisecond).Round(time.Second).String()
		} else if c.State == "running" {
			took = time.Since(time.UnixMilli(c.StartedAt)).Round(time.Second).String() + " so far"
		}
		line := strings.Join(strings.Fields(c.Cmd), " ")
		if r := []rune(line); len(r) > 60 {
			line = string(r[:59]) + "…"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, commandState(&c), ago(c.StartedAt), took, humanBytes(c.OutputBytes), line)
	}
	tw.Flush()
}

func commandState(c *api.Command) string {
	switch {
	case c.State == "exited" && c.ExitCode != nil:
		return fmt.Sprintf("exited %d", *c.ExitCode)
	case c.State == "killed" && c.TimedOut:
		return "timed out"
	case c.State == "killed" && c.Signal != nil:
		return "killed (" + *c.Signal + ")"
	}
	return c.State
}

// pickCommand reads [sandbox] <id>.
func pickCommand(opts *options, args []string) (*api.Client, string, string, error) {
	cfg, client, err := opts.load()
	if err != nil {
		return nil, "", "", err
	}
	r, err := ref(cfg, optionalRef(args[:len(args)-1]))
	if err != nil {
		return nil, "", "", err
	}
	return client, r, args[len(args)-1], nil
}

func newCommandsLogsCmd(opts *options) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs [sandbox] <id>",
		Short: "Print a background command's output",
		Long: "Print a background command's output: standard output and standard error\n" +
			"together, as written. With -f, keep printing until the command ends, then exit\n" +
			"with its exit code; Ctrl-C stops following, and the command keeps running.",
		Example: "  cloudex commands logs cm-1a2b3c4d\n  cloudex commands logs -f my-box cm-1a2b3c4d",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, r, id, err := pickCommand(opts, args)
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			var after int64
			c, err := followCommand(ctx, client, r, id, &after, follow, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if err != nil {
				if ctx.Err() != nil && cmd.Context().Err() == nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "\nStopped following %s; it keeps running.\n", id)
					return nil
				}
				if follow && lostTouch(err) {
					fmt.Fprintf(cmd.ErrOrStderr(), "Follow %s again with: cloudex commands logs -f %s %s\n", id, r, id)
				}
				return err
			}
			if follow {
				return commandExit(c)
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing until the command ends")
	return cmd
}

// followCommand prints a command's output from byte *after, moving it on;
// with follow, it waits for more until the command ends. Once it has ended,
// it reads only what the log held then, so something the command left running
// can't keep it going. A connection that breaks is tried again for up to two
// minutes.
func followCommand(ctx context.Context, client *api.Client, r, id string, after *int64, follow bool, w, errw io.Writer) (*api.Command, error) {
	var failingSince time.Time
	end := int64(-1) // the log's size when the command was first seen ended
	for {
		wait := 0
		if follow {
			wait = commandWaitSec
		}
		out, err := client.CommandOutput(ctx, r, id, *after, wait)
		if err != nil {
			var apiErr *api.Error
			if ctx.Err() != nil || errors.As(err, &apiErr) && apiErr.Status < 500 {
				return nil, err
			}
			if failingSince.IsZero() {
				failingSince = time.Now()
				fmt.Fprintf(errw, "cloudex: lost touch with the sandbox (%v); trying again\n", err)
			} else if time.Since(failingSince) > commandRetryFor {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(commandRetryEvery):
			}
			continue
		}
		failingSince = time.Time{}
		if _, err := io.WriteString(w, out.Data); err != nil {
			return nil, err
		}
		progress := out.Next > *after
		*after = out.Next
		c := out.Command
		if c.State != "running" && end < 0 {
			end = c.OutputBytes
		}
		switch {
		case end >= 0 && (*after >= end || !progress):
			return &c, nil
		case !follow && (*after >= c.OutputBytes || !progress):
			// What's there so far; a character cut off at the end waits.
			return &c, nil
		}
	}
}

// commandExit is how a followed command ended, as this process's exit.
func commandExit(c *api.Command) error {
	switch {
	case c.TimedOut:
		return exitError{124} // as GNU timeout says it
	case c.State == "exited" && c.ExitCode != nil && *c.ExitCode != 0:
		return exitError{*c.ExitCode}
	case c.State == "killed":
		sig := deref(c.Signal)
		if n, ok := signalNumbers[sig]; ok {
			return exitError{128 + n}
		}
		if n, err := strconv.Atoi(sig); err == nil { // a real-time signal
			return exitError{128 + n}
		}
		return exitError{137}
	case c.State == "lost":
		return errors.New("the command was cut off when the sandbox stopped")
	}
	return nil
}

// Linux signal numbers by the names the sandbox reports, as bash adds 128 to them.
var signalNumbers = map[string]int{
	"HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "BUS": 7, "FPE": 8, "KILL": 9,
	"USR1": 10, "SEGV": 11, "USR2": 12, "PIPE": 13, "ALRM": 14, "TERM": 15, "STKFLT": 16, "CHLD": 17,
	"CONT": 18, "STOP": 19, "TSTP": 20, "TTIN": 21, "TTOU": 22, "URG": 23, "XCPU": 24, "XFSZ": 25,
	"VTALRM": 26, "PROF": 27, "WINCH": 28, "IO": 29, "POLL": 29, "PWR": 30, "SYS": 31,
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func newCommandsKillCmd(opts *options) *cobra.Command {
	var sig string
	cmd := &cobra.Command{
		Use:   "kill [sandbox] <id>",
		Short: "Stop a background command and everything it started",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, r, id, err := pickCommand(opts, args)
			if err != nil {
				return err
			}
			c, err := client.KillCommand(cmd.Context(), r, id, strings.ToUpper(strings.TrimPrefix(strings.ToUpper(sig), "SIG")))
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), c)
			}
			if c.State == "running" {
				where := c.ID
				if len(args) > 1 {
					where = args[0] + " " + c.ID
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Sent %s to %s; it's still running. Stop it for sure with: cloudex commands kill --signal KILL %s\n", sig, c.ID, where)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s has ended: %s.\n", c.ID, commandState(c))
			return nil
		},
	}
	cmd.Flags().StringVar(&sig, "signal", "TERM", "TERM, INT, HUP or KILL")
	return cmd
}

func newCommandsRmCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "rm [sandbox] <id>",
		Short: "Remove a finished background command and its output",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, r, id, err := pickCommand(opts, args)
			if err != nil {
				return err
			}
			c, err := client.ForgetCommand(cmd.Context(), r, id)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), c)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed %s and its output.\n", c.ID)
			return nil
		},
	}
}
