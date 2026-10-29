package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newLoginCmd(opts *options) *cobra.Command {
	var url string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Use an existing Cloudex deployment from this computer",
		Long: "Point this computer at a deployment made elsewhere, without redeploying (which\n" +
			"would replace its API key). On a computer that already uses it, cloudex status\n" +
			"--show-key prints the URL and key. The key is read from $CLOUDEX_API_KEY, from\n" +
			"standard input, or asked for.",
		Example: "  cloudex login --url https://cloudex-api.example.workers.dev",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url = strings.TrimRight(firstNonEmpty(url, opts.apiURL, os.Getenv("CLOUDEX_API_URL")), "/")
			if url == "" {
				return errors.New("give the deployment's API URL: cloudex login --url https://cloudex-api.<subdomain>.workers.dev")
			}
			key, err := apiKey(cmd)
			if err != nil {
				return err
			}
			client := api.New(url, key)
			h, err := client.Health(cmd.Context())
			if err != nil {
				return err
			}
			if _, err := client.List(cmd.Context()); err != nil {
				var apiErr *api.Error
				if errors.As(err, &apiErr) && apiErr.Status == 401 {
					return fmt.Errorf("that key doesn't work for %s", url)
				}
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			if cfg.APIURL != url {
				*cfg = config.Config{} // a different deployment: drop its account and current sandbox
			}
			cfg.APIURL, cfg.APIKey, cfg.PreviewZone = url, key, ""
			if h.PreviewZone != nil {
				cfg.PreviewZone = *h.PreviewZone
			}
			if err := cfg.Save(); err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"apiUrl": url, "apiVersion": h.Version, "previewZone": cfg.PreviewZone})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Logged in to %s (version %s).\n", url, h.Version)
			return err
		},
	}
	cmd.Flags().StringVar(&url, "url", "", "the deployment's API URL")
	return cmd
}

// apiKey reads the API key from $CLOUDEX_API_KEY, piped standard input, or a hidden prompt.
func apiKey(cmd *cobra.Command) (string, error) {
	if k := strings.TrimSpace(os.Getenv("CLOUDEX_API_KEY")); k != "" {
		return k, nil
	}
	if !isTerminal(os.Stdin) {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if k := strings.TrimSpace(line); k != "" {
			return k, nil
		}
		return "", errors.New("no API key: set CLOUDEX_API_KEY or pipe the key in")
	}
	fmt.Fprint(cmd.ErrOrStderr(), "API key (it isn't shown): ")
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	if k := strings.TrimSpace(string(b)); k != "" {
		return k, nil
	}
	return "", errors.New("no API key given")
}
