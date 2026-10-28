package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

// Starting can include pulling the image onto a fresh host; saving a full disk
// took about 3 minutes in the spikes.
const lifecycleTimeout = 20 * time.Minute

func optionalRef(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func newNewCmd(opts *options) *cobra.Command {
	var name, size string
	var idle time.Duration
	var noUse bool
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create and start a sandbox",
		Long: "Create a sandbox and make it the current one. It stops (and saves its disk) after\n" +
			"--idle without SSH, exec, preview or agent activity.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			req := api.CreateRequest{Name: name, Size: size}
			if cmd.Flags().Changed("idle") {
				minutes := int(idle.Minutes())
				req.IdleMinutes = &minutes
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "Starting a sandbox. The first start on a host can take a minute while it fetches the image.")
			ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
			defer cancel()
			s, err := client.Create(ctx, req)
			if err != nil {
				return err
			}
			if !noUse {
				cfg.Current = s.ID
				if err := cfg.Save(); err != nil {
					return err
				}
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), s)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is running (%s).\n", s.Label(), s.Size)
			if !noUse {
				fmt.Fprintln(cmd.OutOrStdout(), "It is now the current sandbox. Connect with: cloudex ssh")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "a name to use instead of the id")
	cmd.Flags().StringVar(&size, "size", "", "lite, standard-1, standard-2 (default), standard-3 or standard-4")
	cmd.Flags().DurationVar(&idle, "idle", 30*time.Minute, "stop after this long without activity; 0 waits for the 6-hour platform limit")
	cmd.Flags().BoolVar(&noUse, "no-use", false, "don't make it the current sandbox")
	return cmd
}

func newListCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List sandboxes",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			list, err := client.List(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				if list == nil {
					list = []api.Sandbox{}
				}
				return writeJSON(cmd.OutOrStdout(), list)
			}
			if len(list) == 0 {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "No sandboxes yet. Create one with: cloudex new")
				return err
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "  ID\tNAME\tSTATE\tSIZE\tSAVED\tCREATED")
			for _, s := range list {
				mark := " "
				if s.ID == cfg.Current || (s.Name != nil && *s.Name == cfg.Current) {
					mark = "*"
				}
				name := "-"
				if s.Name != nil {
					name = *s.Name
				}
				saved := "-"
				if s.SavedAt != nil {
					saved = ago(*s.SavedAt)
				}
				fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\t%s\n", mark, s.ID, name, s.State, s.Size, saved, ago(s.CreatedAt))
			}
			return tw.Flush()
		},
	}
}

func newInfoCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "info [sandbox]",
		Short: "Show a sandbox's details",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, optionalRef(args))
			if err != nil {
				return err
			}
			s, err := client.Get(cmd.Context(), r)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), s)
			}
			printSandbox(cmd.OutOrStdout(), s)
			return nil
		},
	}
}

func printSandbox(w io.Writer, s *api.Sandbox) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "ID\t%s\n", s.ID)
	if s.Name != nil {
		fmt.Fprintf(tw, "Name\t%s\n", *s.Name)
	}
	fmt.Fprintf(tw, "State\t%s\n", s.State)
	fmt.Fprintf(tw, "Size\t%s\n", s.Size)
	if s.IdleMinutes > 0 {
		fmt.Fprintf(tw, "Idle stop\tafter %d minutes\n", s.IdleMinutes)
	} else {
		fmt.Fprintf(tw, "Idle stop\toff (6-hour platform limit)\n")
	}
	fmt.Fprintf(tw, "Created\t%s\n", ago(s.CreatedAt))
	if s.StartedAt != nil && s.State == "running" {
		fmt.Fprintf(tw, "Started\t%s\n", ago(*s.StartedAt))
	}
	if s.SavedAt != nil {
		size := ""
		if s.SavedBytes != nil {
			size = fmt.Sprintf(" (%s of changes)", humanBytes(*s.SavedBytes))
		}
		fmt.Fprintf(tw, "Saved\t%s%s\n", ago(*s.SavedAt), size)
	}
	if s.Error != nil {
		fmt.Fprintf(tw, "Note\t%s\n", *s.Error)
	}
	tw.Flush()
}

func newUseCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "use <sandbox>",
		Short: "Make a sandbox the current one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			s, err := client.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			cfg.Current = s.ID
			if err := cfg.Save(); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is now the current sandbox.\n", s.Label())
			return err
		},
	}
}

// lifecycleCmd builds stop and resume, which share their shape.
func lifecycleCmd(opts *options, use, short, doing string, call func(*api.Client, context.Context, string) (*api.Sandbox, error), done string) *cobra.Command {
	return &cobra.Command{
		Use:   use + " [sandbox]",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, optionalRef(args))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%s %s...\n", doing, r)
			ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
			defer cancel()
			s, err := call(client, ctx, r)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), s)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s %s.\n", s.Label(), done)
			return err
		},
	}
}

func newStopCmd(opts *options) *cobra.Command {
	return lifecycleCmd(opts, "stop", "Save a sandbox's disk and stop it", "Saving and stopping",
		(*api.Client).Stop, "is stopped; its disk is saved")
}

func newResumeCmd(opts *options) *cobra.Command {
	return lifecycleCmd(opts, "resume", "Start a stopped sandbox from its saved disk", "Resuming",
		(*api.Client).Resume, "is running")
}

func newDeleteCmd(opts *options) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "delete [sandbox]",
		Aliases: []string{"rm"},
		Short:   "Delete a sandbox for good",
		Long: "Delete a sandbox. Its files can't be recovered afterwards. Cloudflare removes the\n" +
			"last saved disk from your account's registry within 30 days.",
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
			if !yes {
				if !isTerminal(os.Stdin) {
					return errors.New("deleting can't be undone; pass --yes to confirm")
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Delete %s? Its files can't be recovered. [y/N] ", r)
				answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
					return errors.New("not deleted")
				}
			}
			s, err := client.Get(cmd.Context(), r)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
			defer cancel()
			if err := client.Delete(ctx, s.ID); err != nil {
				return err
			}
			if cfg.Current == s.ID || cfg.Current == r {
				cfg.Current = ""
				if err := cfg.Save(); err != nil {
					return err
				}
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"deleted": s.ID})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s.\n", s.Label())
			return err
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return cmd
}

// ago formats a millisecond timestamp as a rough age.
func ago(ms int64) string {
	d := time.Since(time.UnixMilli(ms))
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
