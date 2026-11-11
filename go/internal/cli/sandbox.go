package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

// The API allows a stop 15 minutes to save and 20 to back up, and a start can
// pull the image first.
var lifecycleTimeout = 40 * time.Minute

type durationFlag struct{ d *time.Duration }

func (f durationFlag) Set(s string) error {
	d, err := time.ParseDuration(s)
	if err != nil {
		if _, numErr := strconv.ParseFloat(s, 64); numErr == nil {
			return fmt.Errorf("add a unit, such as %sm or %sh", s, s)
		}
		return err
	}
	*f.d = d
	return nil
}

func (f durationFlag) String() string {
	if f.d == nil || *f.d == 0 {
		return "0"
	}
	return f.d.String()
}

func (durationFlag) Type() string { return "duration" }

func display(ctx context.Context, client *api.Client, r string) string {
	if sandboxIDRE.MatchString(r) {
		if s, err := client.Get(ctx, r); err == nil {
			return s.Label()
		}
	}
	return r
}

func needRunning(s *api.Sandbox) error {
	switch s.State {
	case "running":
		return nil
	case "starting", "stopping":
		return fmt.Errorf("%s is %s; try again in a moment", s.Label(), s.State)
	}
	return fmt.Errorf("%s is %s; start it with: cloudex resume %s", s.Label(), s.State, s.Label())
}

func optionalRef(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// parseMemory reads --memory, such as 4G, 4GiB, 512M or 512MiB, as MiB.
func parseMemory(s string) (int, error) {
	t := strings.ToUpper(strings.TrimSpace(s))
	unit := 0
	for _, u := range []struct {
		suffix string
		mib    int
	}{{"GIB", 1024}, {"GB", 1024}, {"G", 1024}, {"MIB", 1}, {"MB", 1}, {"M", 1}} {
		if strings.HasSuffix(t, u.suffix) {
			t, unit = strings.TrimSuffix(t, u.suffix), u.mib
			break
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
	if unit == 0 || err != nil || v <= 0 {
		return 0, fmt.Errorf("bad --memory %q: give it with a unit, such as 4G or 512M", s)
	}
	return int(v * float64(unit)), nil
}

// whereText says where a sandbox runs: cloudflare, or its node's name, with
// whether that node is offline or was removed.
func whereText(s *api.Sandbox) string {
	n := s.OnNode()
	if n == nil {
		return "cloudflare"
	}
	switch {
	case n.State == nil:
		return clean(n.Name) + " (removed)"
	case *n.State == "revoked":
		return clean(n.Name) + " (removed)"
	case !n.Online:
		return clean(n.Name) + " (offline)"
	}
	return clean(n.Name)
}

func newNewCmd(opts *options) *cobra.Command {
	var name, size, from, repo, on, mem string
	var cpus float64
	var count int
	idle := 30 * time.Minute
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
			"settings (see cloudex fork), or from a named save (see cloudex save). When a\n" +
			"sandbox and a save share a name, the sandbox wins; --from save:<name> picks the save.\n\n" +
			"With --on, it runs on one of your nodes (see cloudex node) instead of Cloudflare,\n" +
			"with --cpus and --memory as its machine's limits there. A sandbox on a node keeps\n" +
			"its disk there when it stops, and has no 6-hour limit. --on goes with --from a\n" +
			"named save, which moves a sandbox between Cloudflare and a node; forks aren't\n" +
			"available on nodes yet.",
		Example: "  cloudex new --name my-box\n" +
			"  cloudex new --on box --cpus 2 --memory 4G --name n1\n" +
			"  cloudex new --repo https://github.com/you/app --env GH_TOKEN=$(gh auth token) --env-file .env\n" +
			"  cloudex new --from my-box --count 3 --name agent\n" +
			"  cloudex new --from base --name task-1\n" +
			"  cloudex new --from base --on box --name n1",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			settingsFlags := len(envPairs)+len(envFiles)+len(files) > 0 || repo != ""
			if on == "" && (cmd.Flags().Changed("cpus") || cmd.Flags().Changed("memory")) {
				return errors.New("--cpus and --memory go with --on: on Cloudflare, pick a --size instead")
			}
			if on == "" && from != "" && (cmd.Flags().Changed("cpus") || cmd.Flags().Changed("memory")) {
				return errors.New("--cpus and --memory go with --on")
			}
			if from != "" && settingsFlags {
				return errors.New("--env, --env-file, --file and --repo don't go with --from: the new sandboxes get the settings of the sandbox or save; change them after with cloudex env set")
			}
			if from != "" {
				req, err := forkRequest(cmd, name, size, count, idle)
				if err != nil {
					return err
				}
				if on != "" {
					req.Node = on
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
				}
				return runNewFrom(cmd, opts, from, req, noUse)
			}
			if cmd.Flags().Changed("count") {
				return errors.New("--count needs --from: it starts several sandboxes from one sandbox or save")
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
			req := api.CreateRequest{Name: name, Size: size, Repo: repo, Node: on}
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
			if len(env) > 0 {
				req.Env = env
			}
			if len(fileMap) > 0 {
				req.Files = fileMap
			}
			if cmd.Flags().Changed("idle") {
				minutes, err := idleMinutes(idle)
				if err != nil {
					return err
				}
				req.IdleMinutes = &minutes
			}
			if on != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Starting a sandbox on node %s.\n", on)
			} else {
				fmt.Fprintln(cmd.ErrOrStderr(), "Starting a sandbox. The first start on a host can take a minute while it fetches the image.")
			}
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
			if n := s.OnNode(); n != nil {
				fmt.Fprintf(cmd.OutOrStdout(), "%s is running on node %s (%s).\n", s.Label(), clean(n.Name), limits(s))
				if s.Isolation != nil && (*s.Isolation == "docker" || *s.Isolation == "none") {
					fmt.Fprintln(cmd.ErrOrStderr(), isolationWarning(clean(n.Name), *s.Isolation))
				}
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s is running (%s).\n", s.Label(), s.Size)
			}
			if s.Notice != nil && *s.Notice != "" {
				fmt.Fprintln(cmd.OutOrStdout(), api.CleanText(*s.Notice))
			}
			if !noUse {
				fmt.Fprintln(cmd.OutOrStdout(), "It is now the current sandbox. Connect with: cloudex ssh")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "a name to use instead of the id")
	cmd.Flags().StringVar(&size, "size", "", "standard-1, standard-2 (default), standard-3 or standard-4")
	cmd.Flags().Var(durationFlag{&idle}, "idle", "stop after this long without activity, such as 30m or 2h; 0 never stops it on a node, and waits for the 6-hour platform limit on Cloudflare")
	cmd.Flags().BoolVar(&noUse, "no-use", false, "don't make it the current sandbox")
	cmd.Flags().StringVar(&on, "on", "", "run it on this node (your own Docker host; see cloudex node) instead of Cloudflare")
	cmd.Flags().Float64Var(&cpus, "cpus", 0, "with --on: its CPUs, such as 0.5 or 4 (default the size's: 1)")
	cmd.Flags().StringVar(&mem, "memory", "", "with --on: its memory, such as 4G or 512M (default the size's: 6G, at most half the node's)")
	cmd.Flags().StringVar(&from, "from", "", "start from a copy of this sandbox's disk (a fork), or from this named save")
	cmd.Flags().IntVar(&count, "count", 1, fmt.Sprintf("with --from, how many sandboxes to start, up to %d", maxForks))
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
			fmt.Fprintln(tw, "  ID\tNAME\tSTATE\tWHERE\tSIZE\tSAVED\tCREATED")
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
				size := s.Size
				if s.OnNode() != nil {
					size = "-" // its CPUs and memory: cloudex info
				}
				fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\t%s\t%s\n", mark, s.ID, name, s.State, whereText(&s), size, saved, ago(s.CreatedAt))
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
	n := s.OnNode()
	if n != nil {
		w := "node " + whereText(s)
		if n.State != nil && *n.State == "offline" && n.LastSeenAt != nil {
			w += ", last seen " + ago(*n.LastSeenAt)
		}
		fmt.Fprintf(tw, "Where\t%s\n", w)
		fmt.Fprintf(tw, "Machine\t%s\n", limits(s))
	} else {
		if s.Placement != nil {
			fmt.Fprintf(tw, "Where\tcloudflare\n")
		}
		fmt.Fprintf(tw, "Size\t%s\n", s.Size)
	}
	if s.Isolation != nil {
		fmt.Fprintf(tw, "Isolation\t%s\n", sandboxIsolation(*s.Isolation))
	}
	switch {
	case s.IdleMinutes > 0:
		fmt.Fprintf(tw, "Idle stop\tafter %d minutes\n", s.IdleMinutes)
	case n != nil:
		fmt.Fprintf(tw, "Idle stop\toff\n")
	default:
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
	if s.FromSave != nil {
		fmt.Fprintf(tw, "From save\t%s\n", *s.FromSave)
	}
	if len(s.ProviderKeys) > 0 && s.State == "running" {
		fmt.Fprintf(tw, "Provider keys\t%s, added on the way out\n", strings.Join(s.ProviderKeys, ", "))
	}
	if s.Notice != nil {
		fmt.Fprintf(tw, "Note\t%s\n", clean(*s.Notice))
	}
	if s.Error != nil {
		fmt.Fprintf(tw, "Error\t%s\n", clean(*s.Error))
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
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), s)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is now the current sandbox.\n", s.Label())
			return err
		},
	}
}

// lifecycleCmd builds stop and resume, which share their shape.
func lifecycleCmd(opts *options, use, short, doing string, call func(*api.Client, context.Context, string) (*api.Sandbox, error), done func(*api.Sandbox) string) *cobra.Command {
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
			shown := display(cmd.Context(), client, r)
			ctx, cancel := context.WithTimeout(cmd.Context(), lifecycleTimeout)
			defer cancel()
			saying := doing
			if use == "stop" {
				// A sandbox on a node keeps its disk there: nothing is saved.
				gctx, gcancel := context.WithTimeout(ctx, 15*time.Second)
				if s, err := client.Get(gctx, r); err == nil && s.OnNode() != nil {
					saying = "Stopping"
				}
				gcancel()
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%s %s...\n", saying, shown)
			s, err := call(client, ctx, r)
			if err != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return fmt.Errorf("gave up waiting after %d minutes, but the work on %s goes on. Check on it with: cloudex info %s", int(lifecycleTimeout.Minutes()), shown, shown)
				}
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), s)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s.\n", s.Label(), done(s))
			if s.Error != nil {
				fmt.Fprintln(cmd.OutOrStdout(), api.CleanText(*s.Error))
			}
			if use == "resume" && s.Notice != nil {
				fmt.Fprintln(cmd.OutOrStdout(), api.CleanText(*s.Notice))
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
		(*api.Client).Stop, func(s *api.Sandbox) string {
			if s.State == "stopped" && s.OnNode() != nil {
				return "is stopped; its disk stays on node " + clean(s.OnNode().Name)
			}
			if s.State == "stopped" {
				return "is stopped; its disk is saved"
			}
			return stateIs(s)
		})
}

func stateIs(s *api.Sandbox) string {
	if s.State == "error" {
		return "is in an error state"
	}
	return "is " + s.State
}

func newResumeCmd(opts *options) *cobra.Command {
	var fromBackup bool
	var size string
	cmd := lifecycleCmd(opts, "resume", "Start a stopped sandbox from its saved disk", "Resuming",
		func(c *api.Client, ctx context.Context, ref string) (*api.Sandbox, error) {
			s, err := c.Resume(ctx, ref, api.ResumeRequest{FromBackup: fromBackup, Size: size})
			if err == nil && size != "" && s.Size != size {
				// A deployment from before v0.1.14 ignores the size.
				return nil, fmt.Errorf("%s is running, but on %s: this deployment can't change sizes yet; update it with cloudex deploy", s.Label(), s.Size)
			}
			return s, err
		}, stateIs)
	cmd.Long = "Start a stopped sandbox from its saved disk. A save expires after 30 days without\n" +
		"a resume; after that, resume restores /home from the newest backup onto a fresh\n" +
		"image instead, and anything installed outside /home needs installing again.\n\n" +
		"With --size, the sandbox runs on that size from then on. Its disk must fit the\n" +
		"new size's (standard-1 8 GB, standard-2 12 GB, standard-3 16 GB, standard-4 20 GB)."
	cmd.Example = "  cloudex resume\n  cloudex resume my-box --size standard-4"
	cmd.Flags().BoolVar(&fromBackup, "from-backup", false, "restore /home from the newest backup onto a fresh image instead of using the save")
	cmd.Flags().StringVar(&size, "size", "", "run on this size from now on: standard-1, standard-2, standard-3 or standard-4")
	return cmd
}

// idleMinutes turns --idle into the API's whole minutes, from 1 to 360, or 0 for off.
func idleMinutes(d time.Duration) (int, error) {
	if d != 0 && (d < time.Minute || d > 6*time.Hour) {
		return 0, errors.New("--idle must be 0, or from 1m to 6h")
	}
	return int(d.Minutes()), nil
}

func newSetCmd(opts *options) *cobra.Command {
	var name string
	var idle time.Duration
	cmd := &cobra.Command{
		Use:   "set [sandbox]",
		Short: "Rename a sandbox, or change how long it may sit idle",
		Long: "Change a sandbox's name, or how long it may go without SSH, exec, preview or\n" +
			"agent activity before it stops and saves its disk. A new idle time applies at\n" +
			"once, counting from now; 0 keeps the sandbox up until the platform's 6-hour\n" +
			"limit on Cloudflare, and for good on a node. To change its size, stop it, then\n" +
			"resume it with --size.",
		Example: "  cloudex set --idle 2h\n  cloudex set my-box --name web",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var req api.UpdateRequest
			if cmd.Flags().Changed("name") {
				req.Name = &name
			}
			if cmd.Flags().Changed("idle") {
				minutes, err := idleMinutes(idle)
				if err != nil {
					return err
				}
				req.IdleMinutes = &minutes
			}
			if req.Name == nil && req.IdleMinutes == nil {
				return errors.New("give --name or --idle")
			}
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, optionalRef(args))
			if err != nil {
				return err
			}
			var renamed *string
			if req.Name != nil {
				before, err := client.Get(cmd.Context(), r)
				if err != nil {
					return err
				}
				renamed = before.Name
			}
			s, err := client.Update(cmd.Context(), r, req)
			if err != nil {
				return err
			}
			if renamed != nil && *renamed != *req.Name {
				forgetSSHName(*renamed)
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), s)
			}
			printSandbox(cmd.OutOrStdout(), s)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "a new name")
	cmd.Flags().Var(durationFlag{&idle}, "idle", "stop after this long without activity, such as 30m or 2h; 0 never stops it on a node, and waits for the 6-hour platform limit on Cloudflare")
	return cmd
}

func newDeleteCmd(opts *options) *cobra.Command {
	var yes, force bool
	cmd := &cobra.Command{
		Use:     "delete [sandbox]",
		Aliases: []string{"rm"},
		Short:   "Delete a sandbox for good",
		Long: "Delete a sandbox. Its files can't be recovered afterwards. Cloudflare removes the\n" +
			"last saved disk from your account's registry within 30 days. On a node, its machine\n" +
			"is removed there; while the node is offline that's refused, unless --force: then the\n" +
			"sandbox goes at once, and the node removes the machine when it's back.",
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
			del := client.Delete
			if force {
				del = client.DeleteForce
			}
			if err := del(ctx, s.ID); err != nil {
				return err
			}
			if s.Name != nil {
				forgetSSHName(*s.Name)
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
	cmd.Flags().BoolVar(&force, "force", false, "on a node that's offline, delete it anyway; the node removes the machine when it's back")
	return cmd
}

// limits is a node sandbox's machine: its CPUs and memory.
func limits(s *api.Sandbox) string {
	if s.Placement == nil || s.Placement.CPUs == nil || s.Placement.MemoryMB == nil {
		return "-"
	}
	c := *s.Placement.CPUs
	unit := "CPUs"
	if c == 1 {
		unit = "CPU"
	}
	return fmt.Sprintf("%s %s, %.1f GiB", strconv.FormatFloat(c, 'f', -1, 64), unit, float64(*s.Placement.MemoryMB)/1024)
}

// sandboxIsolation says how a sandbox is kept apart (Sandbox.isolation).
func sandboxIsolation(iso string) string {
	switch iso {
	case "vm":
		return "vm (Cloudflare)"
	case "sysbox":
		return "sysbox"
	case "docker":
		return "docker (trusted code only)"
	case "none":
		return "none (testing only)"
	}
	return clean(iso)
}

// isolationWarning is what new --on says on a node whose machines run under
// plain Docker, or with no isolation at all.
func isolationWarning(node, iso string) string {
	if iso == "none" {
		return "Node " + node + " joined with --unisolated, for testing only: machines on it reach the host\n" +
			"and its network. Use it only for code you trust."
	}
	return "Machines on " + node + " run under plain Docker. Root in a machine is root on " + node + " if\n" +
		"code escapes the container. Use this node only for code you trust."
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
