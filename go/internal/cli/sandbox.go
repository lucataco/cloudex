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
	var name, size, from, repo string
	var count int
	var idle time.Duration
	var noUse bool
	var envPairs, envFiles, files []string
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Create and start a sandbox",
		Long: "Create a sandbox and make it the current one. It stops (and saves its disk) after\n" +
			"--idle without SSH, exec, preview or agent activity.\n\n" +
			"--env, --env-file and --file give it environment variables and files at every\n" +
			"start (see cloudex env), and --repo clones a repo into its home directory.\n\n" +
			"With --from, the new sandboxes start from a copy of another's disk, and its\n" +
			"settings; see cloudex fork.",
		Example: "  cloudex new --name my-box\n" +
			"  cloudex new --repo https://github.com/you/app --env GH_TOKEN=$(gh auth token) --env-file .env\n" +
			"  cloudex new --from my-box --count 3 --name agent",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			settingsFlags := len(envPairs)+len(envFiles)+len(files) > 0 || repo != ""
			if from != "" && settingsFlags {
				return errors.New("--env, --env-file, --file and --repo don't go with --from: forks get the original's settings; change them after with cloudex env set")
			}
			if from != "" {
				req, err := forkRequest(cmd, name, size, count, idle)
				if err != nil {
					return err
				}
				return runFork(cmd, opts, from, req, noUse)
			}
			if cmd.Flags().Changed("count") {
				return errors.New("--count needs --from: it makes several forks of one sandbox")
			}
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			env, err := envSettings(envPairs, envFiles)
			if err != nil {
				return err
			}
			fileMap, err := fileSettings(files)
			if err != nil {
				return err
			}
			req := api.CreateRequest{Name: name, Size: size, Repo: repo}
			if len(env) > 0 {
				req.Env = env
			}
			if len(fileMap) > 0 {
				req.Files = fileMap
			}
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
			if s.Notice != nil && *s.Notice != "" {
				fmt.Fprintln(cmd.OutOrStdout(), *s.Notice)
			}
			if !noUse {
				fmt.Fprintln(cmd.OutOrStdout(), "It is now the current sandbox. Connect with: cloudex ssh")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "a name to use instead of the id")
	cmd.Flags().StringVar(&size, "size", "", "standard-1, standard-2 (default), standard-3 or standard-4")
	cmd.Flags().DurationVar(&idle, "idle", 30*time.Minute, "stop after this long without activity; 0 waits for the 6-hour platform limit")
	cmd.Flags().BoolVar(&noUse, "no-use", false, "don't make it the current sandbox")
	cmd.Flags().StringVar(&from, "from", "", "start from a copy of this sandbox's disk (a fork)")
	cmd.Flags().IntVar(&count, "count", 1, fmt.Sprintf("with --from, how many forks to make, up to %d", maxForks))
	cmd.Flags().StringArrayVar(&envPairs, "env", nil, "NAME=VALUE: an environment variable (repeatable)")
	cmd.Flags().StringArrayVar(&envFiles, "env-file", nil, "read NAME=VALUE lines from this file (repeatable)")
	cmd.Flags().StringArrayVar(&files, "file", nil, "DEST=SRC: put the local file SRC at DEST in the sandbox's home (repeatable)")
	cmd.Flags().StringVar(&repo, "repo", "", "an https:// repo to clone into the home directory, with #branch to pick one")
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
	if s.BackupAt != nil {
		size := ""
		if s.BackupBytes != nil {
			size = fmt.Sprintf(" (%s)", humanBytes(*s.BackupBytes))
		}
		fmt.Fprintf(tw, "Backup\t%s%s, of /home\n", ago(*s.BackupAt), size)
	}
	if s.BackupError != nil {
		fmt.Fprintf(tw, "Backup\tthe last one failed: %s\n", *s.BackupError)
	}
	if s.ForkedFrom != nil {
		fmt.Fprintf(tw, "Forked from\t%s\n", *s.ForkedFrom)
	}
	if len(s.ProviderKeys) > 0 && s.State == "running" {
		fmt.Fprintf(tw, "Provider keys\t%s, added on the way out\n", strings.Join(s.ProviderKeys, ", "))
	}
	if s.Notice != nil {
		fmt.Fprintf(tw, "Note\t%s\n", *s.Notice)
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
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s.\n", s.Label(), done)
			if s.Notice != nil {
				fmt.Fprintln(cmd.OutOrStdout(), *s.Notice)
			}
			if use == "stop" && s.BackupError != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Backing up /home failed, so only the save has it: %s\n", *s.BackupError)
			}
			return nil
		},
	}
}

func newStopCmd(opts *options) *cobra.Command {
	return lifecycleCmd(opts, "stop", "Save a sandbox's disk and stop it", "Saving and stopping",
		(*api.Client).Stop, "is stopped; its disk is saved")
}

func newResumeCmd(opts *options) *cobra.Command {
	var fromBackup bool
	cmd := lifecycleCmd(opts, "resume", "Start a stopped sandbox from its saved disk", "Resuming",
		func(c *api.Client, ctx context.Context, ref string) (*api.Sandbox, error) {
			return c.Resume(ctx, ref, fromBackup)
		}, "is running")
	cmd.Long = "Start a stopped sandbox from its saved disk. A save expires after 30 days without\n" +
		"a resume; after that, resume restores /home from the newest backup onto a fresh\n" +
		"image instead, and anything installed outside /home needs installing again."
	cmd.Flags().BoolVar(&fromBackup, "from-backup", false, "restore /home from the newest backup onto a fresh image instead of using the save")
	return cmd
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
