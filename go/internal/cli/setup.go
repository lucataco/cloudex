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

func newStatusCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check the connection to your Cloudex deployment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			h, err := client.Health(cmd.Context())
			if err != nil {
				return err
			}
			zone := ""
			if h.PreviewZone != nil {
				zone = *h.PreviewZone
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string]any{
					"apiUrl": client.BaseURL, "apiVersion": h.Version, "previewZone": zone, "current": cfg.Current,
				})
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "API      %s (version %s)\n", client.BaseURL, h.Version)
			if zone == "" {
				zone = "off (cloudex deploy --preview-domain <domain> turns them on)"
			}
			fmt.Fprintf(w, "Previews %s\n", zone)
			current := cfg.Current
			if current == "" {
				current = "none"
			}
			_, err = fmt.Fprintf(w, "Current  %s\n", current)
			return err
		},
	}
}
