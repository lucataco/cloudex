package cli

import (
	"context"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

const maxForks = 10 // per request, as the API allows

func newForkCmd(opts *options) *cobra.Command {
	var name, size string
	var count int
	var idle time.Duration
	var noUse bool
	cmd := &cobra.Command{
		Use:   "fork [sandbox]",
		Short: "Make new sandboxes that start from a copy of one's disk",
		Long: "Make one or more sandboxes that start from a copy of a sandbox's whole disk:\n" +
			"files, installed packages and settings. A running sandbox is saved first, which\n" +
			"takes a few seconds and leaves it running; a stopped one is copied from its last\n" +
			"save. Each fork is a separate sandbox from then on, with its own id.\n\n" +
			"With --count, the forks are named <name>-1, <name>-2 and so on when --name is\n" +
			"given. A single fork becomes the current sandbox. With --json, prints the list\n" +
			"of forks.",
		Example: "  cloudex fork\n  cloudex fork my-box --name try-2\n  cloudex fork my-box --count 5 --name agent",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := forkRequest(cmd, name, size, count, idle)
			if err != nil {
				return err
			}
			return runFork(cmd, opts, optionalRef(args), req, noUse)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "a name for the fork, or the start of the names with --count")
	cmd.Flags().IntVar(&count, "count", 1, fmt.Sprintf("how many forks to make, up to %d", maxForks))
	cmd.Flags().StringVar(&size, "size", "", "standard-1 to standard-4 (default: the original's)")
	cmd.Flags().DurationVar(&idle, "idle", 0, "stop after this long without activity (default: the original's)")
	cmd.Flags().BoolVar(&noUse, "no-use", false, "don't make the fork the current sandbox")
	return cmd
}

// forkRequest checks the flags shared by fork and new --from.
func forkRequest(cmd *cobra.Command, name, size string, count int, idle time.Duration) (api.ForkRequest, error) {
	if count < 1 || count > maxForks {
		return api.ForkRequest{}, fmt.Errorf("--count must be 1 to %d", maxForks)
	}
	req := api.ForkRequest{Name: name, Count: count, Size: size}
	if cmd.Flags().Changed("idle") {
		minutes := int(idle.Minutes())
		req.IdleMinutes = &minutes
	}
	return req, nil
}

func runFork(cmd *cobra.Command, opts *options, from string, req api.ForkRequest, noUse bool) error {
	cfg, client, err := opts.load()
	if err != nil {
		return err
	}
	r, err := ref(cfg, from)
	if err != nil {
		return err
	}
	what := "a fork"
	if req.Count > 1 {
		what = fmt.Sprintf("%d forks", req.Count)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Making %s of %s. A running sandbox is saved first and keeps running.\n", what, r)
	ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
	defer cancel()
	forks, err := client.Fork(ctx, r, req)
	if err != nil {
		return err
	}
	failed := 0
	for _, f := range forks {
		if f.State != "running" {
			failed++
		}
	}
	if len(forks) == 1 && failed == 0 && !noUse {
		cfg.Current = forks[0].ID
		if err := cfg.Save(); err != nil {
			return err
		}
	}

	out := cmd.OutOrStdout()
	if opts.json {
		if err := writeJSON(out, forks); err != nil {
			return err
		}
	} else if len(forks) == 1 && failed == 0 {
		f := forks[0]
		fmt.Fprintf(out, "%s is running (%s), a fork of %s.\n", f.Label(), f.Size, r)
		if !noUse {
			fmt.Fprintln(out, "It is now the current sandbox. Connect with: cloudex ssh")
		}
	} else {
		tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tSTATE")
		for _, f := range forks {
			name := "-"
			if f.Name != nil {
				name = *f.Name
			}
			state := f.State
			if f.Error != nil {
				state += ": " + *f.Error
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\n", f.ID, name, state)
		}
		tw.Flush()
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d forks didn't start; resume them to try again, or delete them", failed, len(forks))
	}
	return nil
}
