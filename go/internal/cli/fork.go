package cli

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

const maxForks = 10 // per request, as the API allows

func newForkCmd(opts *options) *cobra.Command {
	var name, size, mem string
	var cpus float64
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
			"A fork of a sandbox on a node stays on that node: its disk is copied there, and\n" +
			"a running sandbox pauses while that happens (cloudex node info says about how\n" +
			"long per GiB it has written; stop it first to avoid the pause). --cpus and\n" +
			"--memory set the forks' limits there (default: the original's).\n\n" +
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
			if err := nodeLimits(cmd, &req, cpus, mem); err != nil {
				return err
			}
			return runFork(cmd, opts, optionalRef(args), req, noUse)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "a name for the fork, or the start of the names with --count")
	cmd.Flags().IntVar(&count, "count", 1, fmt.Sprintf("how many forks to make, up to %d", maxForks))
	cmd.Flags().StringVar(&size, "size", "", "standard-1 to standard-4 (default: the original's)")
	cmd.Flags().Var(durationFlag{&idle}, "idle", "stop after this long without activity, such as 30m or 2h (default: the original's)")
	cmd.Flags().BoolVar(&noUse, "no-use", false, "don't make the fork the current sandbox")
	cmd.Flags().Float64Var(&cpus, "cpus", 0, "for a sandbox on a node: the forks' CPUs (default: the original's)")
	cmd.Flags().StringVar(&mem, "memory", "", "for a sandbox on a node: the forks' memory, such as 4G (default: the original's)")
	return cmd
}

// nodeLimits adds --cpus and --memory to a fork request: for a sandbox on a
// node (or a save started on one), which the API checks.
func nodeLimits(cmd *cobra.Command, req *api.ForkRequest, cpus float64, mem string) error {
	if cmd.Flags().Changed("cpus") {
		req.CPUs = &cpus
	}
	if cmd.Flags().Changed("memory") {
		mb, err := parseMemory(mem)
		if err != nil {
			return err
		}
		req.MemoryMB = &mb
	}
	return nil
}

// runNewFrom starts sandboxes for new --from: forks of a sandbox, or
// sandboxes from a named save.
func runNewFrom(cmd *cobra.Command, opts *options, from string, req api.ForkRequest, noUse bool) error {
	_, client, err := opts.load()
	if err != nil {
		return err
	}
	ref, save, err := fromSave(cmd.Context(), client, from)
	if err != nil {
		return err
	}
	if save {
		return runStartFromSave(cmd, opts, ref, req, noUse)
	}
	// A fork stays where its original is; the API says so for --on another node.
	return runFork(cmd, opts, ref, req, noUse)
}

// forkRequest checks the flags shared by fork and new --from.
func forkRequest(cmd *cobra.Command, name, size string, count int, idle time.Duration) (api.ForkRequest, error) {
	if count < 1 || count > maxForks {
		return api.ForkRequest{}, fmt.Errorf("--count must be 1 to %d", maxForks)
	}
	req := api.ForkRequest{Name: name, Count: count, Size: size}
	if cmd.Flags().Changed("idle") {
		minutes, err := idleMinutes(idle)
		if err != nil {
			return api.ForkRequest{}, err
		}
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
	shown := r
	var orig *api.Sandbox
	if s, err := client.Get(cmd.Context(), r); err == nil {
		orig, shown = s, s.Label()
	}
	fmt.Fprintln(cmd.ErrOrStderr(), forkNotice(cmd.Context(), client, orig, what, shown))
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
		if n := f.OnNode(); n != nil {
			fmt.Fprintf(out, "%s is running on node %s (%s), a fork of %s.\n", f.Label(), clean(n.Name), limits(&f), shown)
		} else {
			fmt.Fprintf(out, "%s is running (%s), a fork of %s.\n", f.Label(), f.Size, shown)
		}
		if !noUse {
			fmt.Fprintln(out, "It is now the current sandbox. Connect with: cloudex ssh")
		}
	} else {
		printStarted(out, forks)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d forks didn't start; resume them to try again, or delete them", failed, len(forks))
	}
	return nil
}

// forkNotice is what fork says before it starts: on a node (v0.2.3 M5), that
// the forks stay there and, for a running original, that it pauses while its
// disk is copied, for about how long per GiB on that node.
func forkNotice(ctx context.Context, client *api.Client, orig *api.Sandbox, what, shown string) string {
	n := (*api.PlacementNode)(nil)
	if orig != nil {
		n = orig.OnNode()
	}
	if n == nil {
		return fmt.Sprintf("Making %s of %s. A running sandbox is saved first and keeps running.", what, shown)
	}
	msg := fmt.Sprintf("Making %s of %s on node %s, where its disk is.", what, shown, clean(n.Name))
	if orig.State != "running" {
		return msg
	}
	rate := ""
	if node, err := client.GetNode(ctx, n.ID); err == nil && node.Forks != nil && node.Forks.SecPerGB > 0 {
		rate = fmt.Sprintf(" (about %s s per GiB it has written, on this node's %s image store)", strconv.FormatFloat(node.Forks.SecPerGB, 'f', -1, 64), clean(node.Forks.Store))
	}
	return msg + fmt.Sprintf(" %s keeps running, but is paused while its disk is copied%s; stop it first to avoid the pause.", shown, rate)
}
