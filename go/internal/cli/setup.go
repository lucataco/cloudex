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
	var showKey bool
	cmd := &cobra.Command{
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
				out := map[string]any{"apiUrl": client.BaseURL, "apiVersion": h.Version, "previewZone": zone, "backups": h.Backups, "current": cfg.Current}
				if showKey {
					out["apiKey"] = client.Key
				}
				return writeJSON(cmd.OutOrStdout(), out)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "API      %s (version %s)\n", client.BaseURL, h.Version)
			if showKey {
				fmt.Fprintf(w, "Key      %s\n", client.Key)
			} else {
				fmt.Fprintln(w, "Key      hidden (--show-key prints it, for cloudex login on another computer)")
			}
			if zone == "" {
				zone = "off (cloudex deploy --preview-domain <domain> turns them on)"
			}
			fmt.Fprintf(w, "Previews %s\n", zone)
			switch {
			case h.Backups == nil:
			case *h.Backups:
				fmt.Fprintln(w, "Backups  on: /home goes to the R2 bucket cloudex-backups on each stop")
			default:
				fmt.Fprintln(w, "Backups  off (cloudex deploy turns them on once R2 is enabled for the account)")
			}
			current := cfg.Current
			if current == "" {
				current = "none"
			}
			_, err = fmt.Fprintf(w, "Current  %s\n", current)
			return err
		},
	}
	cmd.Flags().BoolVar(&showKey, "show-key", false, "print the API key too")
	return cmd
}
