package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd(version string, opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"version": version})
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "cloudex", version)
			return err
		},
	}
}
