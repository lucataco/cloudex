// Package cli builds the cloudex command tree.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

type options struct {
	apiURL string
	json   bool
}

// exitError ends the process with a remote command's exit code, without a message.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Run executes the CLI and returns the process exit code. A command that
// fails, or crashes, leaves its error for cloudex feedback --last-error.
func Run(version string, args []string, stdout, stderr io.Writer) (code int) {
	cmd := New(version)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	defer func() {
		if p := recover(); p != nil {
			saveLastError(version, nil, args, nil, fmt.Sprint(p), string(debug.Stack()))
			fmt.Fprintf(stderr, "cloudex crashed: %v\nThat's a bug in Cloudex. Report it with: cloudex feedback --last-error\n", p)
			code = 2
		}
	}()
	notice := startUpdateCheck(version, args, stderr)
	ran, err := cmd.ExecuteC()
	notice()
	var exit exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.code
	default:
		fmt.Fprintln(stderr, "error:", err)
		if ran == nil || ran.Name() != "feedback" {
			saveLastError(version, ran, args, err, err.Error(), "")
			if looksLikeABug(err) {
				fmt.Fprintln(stderr, "If this looks like a bug in Cloudex, report it with: cloudex feedback --last-error")
			}
		}
		return 1
	}
}

// New returns the root command. version is the build's release version.
func New(version string) *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:           "cloudex",
		Short:         "Agent computers in your own Cloudflare account",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&opts.apiURL, "api-url", "", "Cloudex API URL (default from cloudex deploy, or $CLOUDEX_API_URL)")
	root.PersistentFlags().BoolVar(&opts.json, "json", !isTerminal(os.Stdout), "print JSON (the default when stdout is not a terminal)")
	root.AddGroup(
		&cobra.Group{ID: "sandbox", Title: "Sandboxes:"},
		&cobra.Group{ID: "connect", Title: "Connect:"},
		&cobra.Group{ID: "setup", Title: "Setup:"},
	)
	add := func(group string, cmds ...*cobra.Command) {
		for _, c := range cmds {
			c.GroupID = group
			root.AddCommand(c)
		}
	}
	add("sandbox", newNewCmd(opts), newForkCmd(opts), newListCmd(opts), newInfoCmd(opts), newUseCmd(opts), newSetCmd(opts),
		newEnvCmd(opts), newStopCmd(opts), newResumeCmd(opts), newDeleteCmd(opts))
	add("connect", newSSHCmd(opts), newDesktopCmd(opts), newExecCmd(opts), newHostCmd(opts), newPromptCmd(opts),
		newAttachCmd(opts), newAgentsCmd(opts), newSSHConfigCmd(opts))
	add("setup", newDeployCmd(opts), newLoginCmd(opts), newKeysCmd(opts), newDoctorCmd(opts), newStatusCmd(opts),
		newUpdateCmd(version, opts), newVersionCmd(version, opts), newFeedbackCmd(version, opts))
	root.AddCommand(newProxyCmd(opts))
	return root
}

// load returns the config and a client for the configured deployment.
func (o *options) load() (*config.Config, *api.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("reading config: %w", err)
	}
	url := firstNonEmpty(o.apiURL, os.Getenv("CLOUDEX_API_URL"), cfg.APIURL)
	key := firstNonEmpty(os.Getenv("CLOUDEX_API_KEY"), cfg.APIKey)
	if url == "" {
		return nil, nil, errors.New("no Cloudex deployment is set up: run cloudex deploy (or set CLOUDEX_API_URL and CLOUDEX_API_KEY)")
	}
	return cfg, api.New(url, key), nil
}

// ref picks the sandbox a command acts on: the one named, else the current one.
func ref(cfg *config.Config, named string) (string, error) {
	if named != "" {
		return named, nil
	}
	if cfg.Current == "" {
		return "", errors.New("no sandbox given and none is current: name one, or run cloudex use <id or name>")
	}
	return cfg.Current, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// isTerminal is true for a terminal, but not for other character devices
// such as /dev/null.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep & in URLs readable
	return enc.Encode(v)
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
