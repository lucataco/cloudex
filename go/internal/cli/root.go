// Package cli builds the cloudex command tree.
package cli

import (
	"encoding/json"
	"io"
	"os"

	"github.com/spf13/cobra"
)

type options struct {
	apiURL string
	json   bool
}

// New returns the root command. version is the build's release version.
func New(version string) *cobra.Command {
	opts := &options{}
	root := &cobra.Command{
		Use:           "cloudex",
		Short:         "Agent computers in your own Cloudflare account",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().StringVar(&opts.apiURL, "api-url", os.Getenv("CLOUDEX_API_URL"),
		"Cloudex API URL (default $CLOUDEX_API_URL)")
	root.PersistentFlags().BoolVar(&opts.json, "json", !isTerminal(os.Stdout),
		"print JSON (the default when stdout is not a terminal)")
	root.AddCommand(newVersionCmd(version, opts), newStatusCmd(opts))
	return root
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
