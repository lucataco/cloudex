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

// Making or restoring a named save moves about 110 MiB/s, on top of a start.
const saveTimeout = 40 * time.Minute

// The prefix that makes new --from name a save when a sandbox has the name too.
const savePrefix = "save:"

func newSaveCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "save [sandbox] <name>",
		Short: "Save a sandbox's disk under a name, to start new sandboxes from",
		Long: "Save a running sandbox's disk under a name. Start new sandboxes from it with\n" +
			"cloudex new --from <name>.\n\n" +
			"A sandbox's own save goes 30 days after it was last made or resumed from; a named\n" +
			"save stays in your account's R2 bucket until you delete it (cloudex saves rm).\n" +
			"It holds every file that differs from the sandbox image, so installed packages,\n" +
			"Docker images and your files, and the sandbox's settings (cloudex env). SSH host\n" +
			"keys are left out, so each new sandbox makes its own.\n\n" +
			"The sandbox keeps running. Saving under a name that's taken replaces that save\n" +
			"once the new one is done.",
		Example: "  cloudex save base\n  cloudex save my-box node-22",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			name := args[len(args)-1]
			r, err := ref(cfg, optionalRef(args[:len(args)-1]))
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Saving %s as %s. It keeps running; this takes about 15 seconds per GB of changes.\n", r, name)
			ctx, cancel := context.WithTimeout(cmd.Context(), saveTimeout)
			defer cancel()
			s, err := client.SaveAs(ctx, r, name)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), s)
			}
			out := cmd.OutOrStdout()
			what := "every file that differs from the image"
			if s.Kind == "home" {
				what = "/home only: the sandbox's image is from before v0.1.15, whose saves can't hold the rest of the disk"
			}
			fmt.Fprintf(out, "Saved %s as %s: %s in %s, %s stored.\n", r, s.Name, plural(s.Files, "path"), humanBytes(s.RawBytes), humanBytes(s.Bytes))
			fmt.Fprintf(out, "It holds %s.\n", what)
			if s.Replaced != nil && *s.Replaced {
				fmt.Fprintf(out, "It replaces the earlier save of %s.\n", s.Name)
			}
			fmt.Fprintf(out, "Start sandboxes from it with: cloudex new --from %s\n", s.Name)
			return nil
		},
	}
}

func newSavesCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "saves",
		Short: "List named saves, or delete one",
		Long: "List the named saves that new sandboxes can start from (cloudex new --from <name>).\n" +
			"Make one with cloudex save.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			saves, err := client.ListSaves(cmd.Context())
			if err != nil {
				return err
			}
			if opts.json {
				if saves == nil {
					saves = []api.Save{}
				}
				return writeJSON(cmd.OutOrStdout(), saves)
			}
			printSaves(cmd.OutOrStdout(), saves)
			return nil
		},
	}
	cmd.AddCommand(newSavesRmCmd(opts))
	return cmd
}

func printSaves(w io.Writer, saves []api.Save) {
	if len(saves) == 0 {
		fmt.Fprintln(w, "No saves. Make one with: cloudex save [sandbox] <name>")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tHOLDS\tSIZE\tSTORED\tMADE\tFROM")
	for _, s := range saves {
		holds := "disk"
		if s.Kind == "home" {
			holds = "/home"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", s.Name, holds, humanBytes(s.RawBytes), humanBytes(s.Bytes), ago(s.CreatedAt), s.Sandbox)
	}
	tw.Flush()
}

func newSavesRmCmd(opts *options) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:     "rm <name>...",
		Aliases: []string{"delete"},
		Short:   "Delete named saves for good",
		Long:    "Delete named saves. Sandboxes already started from one keep their disks.",
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			if !yes {
				if !isTerminal(os.Stdin) {
					return errors.New("deleting a save can't be undone; pass --yes to confirm")
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Delete the save %s? It can't be recovered. [y/N] ", strings.Join(args, ", "))
				answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
					return errors.New("not deleted")
				}
			}
			var deleted []string
			for _, name := range args {
				if err := client.DeleteSave(cmd.Context(), name); err != nil {
					return err
				}
				deleted = append(deleted, name)
				if !opts.json {
					fmt.Fprintf(cmd.OutOrStdout(), "Deleted the save %s.\n", name)
				}
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string][]string{"deleted": deleted})
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	return cmd
}

// fromSave tells whether new --from names a named save rather than a
// sandbox: save:<name> always does, and so does a name that no sandbox has
// and a save does. It returns the sandbox ref or the save's name.
func fromSave(ctx context.Context, client *api.Client, from string) (string, bool, error) {
	if name, ok := strings.CutPrefix(from, savePrefix); ok {
		return name, true, nil
	}
	_, err := client.Get(ctx, from)
	var notFound *api.Error
	if err == nil || !errors.As(err, &notFound) || notFound.Status != 404 {
		return from, false, err
	}
	if _, err := client.GetSave(ctx, from); err == nil {
		return from, true, nil
	} else if !errors.As(err, &notFound) || notFound.Status != 404 {
		return "", false, err
	}
	return "", false, fmt.Errorf("no sandbox or save named %s", from)
}

func runStartFromSave(cmd *cobra.Command, opts *options, name string, req api.ForkRequest, noUse bool) error {
	cfg, client, err := opts.load()
	if err != nil {
		return err
	}
	what := "a sandbox"
	if req.Count > 1 {
		what = fmt.Sprintf("%d sandboxes", req.Count)
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Starting %s from the save %s. Restoring it takes about 15 seconds per GB.\n", what, name)
	ctx, cancel := context.WithTimeout(cmd.Context(), saveTimeout)
	defer cancel()
	list, err := client.StartFromSave(ctx, name, req)
	if err != nil {
		return err
	}
	failed := 0
	for _, s := range list {
		if s.State != "running" {
			failed++
		}
	}
	if len(list) == 1 && failed == 0 && !noUse {
		cfg.Current = list[0].ID
		if err := cfg.Save(); err != nil {
			return err
		}
	}
	out := cmd.OutOrStdout()
	if opts.json {
		if err := writeJSON(out, list); err != nil {
			return err
		}
	} else if len(list) == 1 && failed == 0 {
		s := list[0]
		fmt.Fprintf(out, "%s is running (%s), from the save %s.\n", s.Label(), s.Size, name)
		if s.Notice != nil && *s.Notice != "" {
			fmt.Fprintln(out, *s.Notice)
		}
		if !noUse {
			fmt.Fprintln(out, "It is now the current sandbox. Connect with: cloudex ssh")
		}
	} else {
		printStarted(out, list)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d sandboxes didn't start; resume them to try again, or delete them", failed, len(list))
	}
	return nil
}

// printStarted lists sandboxes started together, with why any didn't start.
func printStarted(w io.Writer, list []api.Sandbox) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tSTATE")
	for _, s := range list {
		name := "-"
		if s.Name != nil {
			name = *s.Name
		}
		state := s.State
		if s.Error != nil {
			state += ": " + *s.Error
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", s.ID, name, state)
	}
	tw.Flush()
}

func plural(n int64, word string) string {
	switch {
	case n == 1:
		return "1 " + word
	case word == "file and directory":
		return fmt.Sprintf("%d files and directories", n)
	case strings.HasSuffix(word, "y"):
		return fmt.Sprintf("%d %sies", n, strings.TrimSuffix(word, "y"))
	}
	return fmt.Sprintf("%d %ss", n, word)
}
