package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/lucataco/cloudex/go/internal/update"
	"github.com/spf13/cobra"
)

// executable is the file update replaces; tests point it elsewhere.
var executable = self

// isRelease is false for builds from source, which report "dev".
func isRelease(version string) bool {
	return strings.HasPrefix(version, "v")
}

func newUpdateCmd(version string, opts *options) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "update",
		Short: "Update the CLI to the latest release, then your deployment",
		Long: "Download the latest release of the CLI, check it against the release's\n" +
			"checksums and put it in place of this one. Then, if your deployment runs an\n" +
			"older version, offer to update it with cloudex deploy, which keeps your\n" +
			"sandboxes. Sandboxes running during the update keep the old version until\n" +
			"they stop. --check only says what's out of date.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			latest, err := update.Latest(ctx)
			if err != nil {
				return err
			}
			saveLatest(latest)
			exe, err := executable()
			if err != nil {
				return err
			}
			update.CleanUp(exe)

			now := version
			switch {
			case !update.Newer(latest, version):
				fmt.Fprintf(out, "cloudex %s is the latest version.\n", version)
			case check:
				fmt.Fprintf(out, "cloudex %s is out; this is %s. Update with: cloudex update\n", latest, version)
			case !isRelease(version):
				return fmt.Errorf("this cloudex was built from source; build %s the same way, or install it with the install script", latest)
			default:
				fmt.Fprintf(cmd.ErrOrStderr(), "Downloading cloudex %s...\n", latest)
				bin, err := update.Download(ctx, latest)
				if err != nil {
					return err
				}
				if err := update.Replace(exe, bin); err != nil {
					return err
				}
				fmt.Fprintf(out, "Updated cloudex from %s to %s.\n", version, latest)
				now = latest
			}
			return updateDeployment(cmd, opts, exe, now, check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "only say whether the CLI or the deployment is out of date")
	return cmd
}

// updateDeployment offers to bring the deployment up to version by running
// deploy from the CLI at exe, which carries that version's API and image.
func updateDeployment(cmd *cobra.Command, opts *options, exe, version string, check bool) error {
	_, client, err := opts.load()
	if err != nil {
		return nil // this computer has no deployment to update
	}
	out := cmd.OutOrStdout()
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()
	h, err := client.Health(ctx)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Couldn't check your deployment: %v\n", err)
		return nil
	}
	if !isRelease(version) || !update.Newer(version, h.Version) {
		fmt.Fprintf(out, "Your deployment runs %s.\n", h.Version)
		return nil
	}
	question := fmt.Sprintf("Your deployment runs %s. Update it to %s now with cloudex deploy? Sandboxes are kept. [y/N] ", h.Version, version)
	if check || opts.json || !ask(cmd, question) {
		fmt.Fprintf(out, "Your deployment runs %s. Update it with: cloudex deploy\n", h.Version)
		return nil
	}
	deploy := exec.CommandContext(cmd.Context(), exe, "deploy")
	deploy.Stdin, deploy.Stdout, deploy.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := deploy.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exitError{exit.ExitCode()}
		}
		return err
	}
	return nil
}

// The newest release seen, kept so that commands can mention it once a day
// without slowing down.
type updateCheck struct {
	CheckedAt int64  `json:"checkedAt"`
	Latest    string `json:"latest"`
}

func updateCheckPath() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "update-check.json"), nil
}

func saveLatest(latest string) {
	p, err := updateCheckPath()
	if err != nil {
		return
	}
	data, _ := json.Marshal(updateCheck{CheckedAt: time.Now().Unix(), Latest: latest})
	os.WriteFile(p, data, 0o600)
}

// startUpdateCheck looks for a newer release at most once a day, in the
// background, when a person is at the terminal. The function it returns
// prints a one-line notice if there is one.
func startUpdateCheck(version string, args []string, stderr io.Writer) func() {
	f, ok := stderr.(*os.File)
	if !ok || !isTerminal(f) || !isRelease(version) || os.Getenv("CLOUDEX_NO_UPDATE_CHECK") != "" {
		return func() {}
	}
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			// proxy talks to ssh; update and version say it themselves.
			if a == "proxy" || a == "update" || a == "version" || a == "completion" || strings.HasPrefix(a, "__") {
				return func() {}
			}
			break
		}
	}
	p, err := updateCheckPath()
	if err != nil {
		return func() {}
	}
	var last updateCheck
	if data, err := os.ReadFile(p); err == nil {
		json.Unmarshal(data, &last)
	}
	done := make(chan struct{})
	if time.Since(time.Unix(last.CheckedAt, 0)) < 24*time.Hour {
		close(done)
	} else {
		go func() {
			defer close(done)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if latest, err := update.Latest(ctx); err == nil {
				saveLatest(latest)
				last.Latest = latest
			}
		}()
	}
	return func() {
		select {
		case <-done:
		case <-time.After(500 * time.Millisecond):
			return // never hold up a command for this
		}
		if update.Newer(last.Latest, version) {
			fmt.Fprintf(stderr, "\ncloudex %s is out (this is %s). Update with: cloudex update\n", last.Latest, version)
		}
	}
}
