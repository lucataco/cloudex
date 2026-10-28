// Package cli builds the cloudex command tree.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/spf13/cobra"
)

type options struct {
	apiURL string
	json   bool
}

// exitError ends the process with a remote command's exit code, without a message.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Run executes the CLI and returns the process exit code.
func Run(version string, args []string, stdout, stderr io.Writer) int {
	cmd := New(version)
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	err := cmd.Execute()
	var exit exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.code
	default:
		fmt.Fprintln(stderr, "error:", err)
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
	add("sandbox", newNewCmd(opts), newListCmd(opts), newInfoCmd(opts), newUseCmd(opts),
		newStopCmd(opts), newResumeCmd(opts), newDeleteCmd(opts))
	add("connect", newSSHCmd(opts), newExecCmd(opts), newHostCmd(opts), newPromptCmd(opts),
		newAttachCmd(opts), newSSHConfigCmd(opts))
	add("setup", newDeployCmd(opts), newDoctorCmd(opts), newStatusCmd(opts), newVersionCmd(version, opts))
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

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
