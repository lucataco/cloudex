package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newLoginCmd(opts *options) *cobra.Command {
	var url, name, role string
	var withKey bool
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Use an existing Cloudex deployment from this computer",
		Long: "Point this computer at a deployment made elsewhere, without redeploying (which\n" +
			"would replace its API key).\n\n" +
			"With no key, it asks the deployment for a token of this computer's own and shows a\n" +
			"code; on a computer that already uses the deployment, approve it with cloudex\n" +
			"tokens approve <code>. A key is used instead when given: from $CLOUDEX_API_KEY,\n" +
			"from standard input, or asked for with --key. The admin key or a token works.",
		Example: "  cloudex login --url https://cloudex-api.example.workers.dev\n" +
			"  cloudex login --url https://cloudex-api.example.workers.dev --key\n" +
			"  echo \"$CLOUDEX_TOKEN\" | cloudex login --url https://cloudex-api.example.workers.dev",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			url = api.NormalizeURL(firstNonEmpty(url, opts.apiURL, os.Getenv("CLOUDEX_API_URL")))
			if url == "" {
				return errors.New("give the deployment's API URL: cloudex login --url https://cloudex-api.<subdomain>.workers.dev")
			}
			key, err := givenKey(cmd, withKey)
			if err != nil {
				return err
			}
			if key == "" {
				if key, err = deviceLogin(cmd, opts, url, name, role); err != nil {
					return err
				}
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
			if api.NormalizeURL(cfg.APIURL) != url {
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
	cmd.Flags().BoolVar(&withKey, "key", false, "ask for a key instead of signing in with a code")
	cmd.Flags().StringVar(&name, "name", "", "what this computer's token is called (default: cloudex on <host name>)")
	cmd.Flags().StringVar(&role, "role", "", "the role to ask for: admin, user or read (the approver decides, and admin only if they choose it)")
	return cmd
}

// deviceLogin asks the deployment for a token with a code (RFC 8628) and
// waits until someone approves it, refuses it, or it expires.
func deviceLogin(cmd *cobra.Command, opts *options, url, name, role string) (string, error) {
	if name == "" {
		host, _ := os.Hostname()
		name = "cloudex on " + firstNonEmpty(strings.TrimSuffix(host, ".local"), "a computer")
	}
	client := api.New(url, "")
	ctx := cmd.Context()
	d, err := client.StartDeviceLogin(ctx, name, role)
	if err != nil {
		return "", err
	}
	approve := "cloudex tokens approve " + d.UserCode
	if opts.json {
		// First, so that an agent can show its user the code while this waits.
		if err := writeJSONLine(cmd.OutOrStdout(), map[string]any{
			"userCode": d.UserCode, "approveWith": approve, "expiresIn": d.ExpiresIn,
		}); err != nil {
			return "", err
		}
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "To sign this computer in, run this on a computer that already uses %s:\n\n  %s\n\n"+
			"Waiting for it (the code works for %d minutes; Ctrl-C to stop)...\n", url, approve, d.ExpiresIn/60)
	}
	interval := time.Duration(max(d.Interval, 1)) * time.Second
	deadline := time.Now().Add(time.Duration(d.ExpiresIn) * time.Second)
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(interval):
		}
		t, err := client.PollDeviceLogin(ctx, d.DeviceCode)
		switch {
		case err == nil:
			if !opts.json {
				fmt.Fprintf(cmd.ErrOrStderr(), "Approved: this computer has its own token, %s (role %s).\n", t.Token.ID, t.Token.Role)
			}
			return t.AccessToken, nil
		case errors.Is(err, api.ErrSlowDown):
			interval += 5 * time.Second
		case errors.Is(err, api.ErrPending):
		default:
			var e *api.Error
			if errors.As(err, &e) && e.Message == "access_denied" {
				return "", errors.New("the sign-in was refused")
			}
			if errors.As(err, &e) && e.Message == "expired_token" {
				return "", errors.New("the code expired before anyone approved it; run cloudex login again")
			}
			return "", err
		}
		if time.Now().After(deadline) {
			return "", errors.New("the code expired before anyone approved it; run cloudex login again")
		}
	}
}

// givenKey reads a key from $CLOUDEX_API_KEY or piped standard input, or from
// a hidden prompt with --key. "" means none: sign in with a code instead.
func givenKey(cmd *cobra.Command, ask bool) (string, error) {
	if k := strings.TrimSpace(os.Getenv("CLOUDEX_API_KEY")); k != "" {
		return k, nil
	}
	if !isTerminal(os.Stdin) {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if k := strings.TrimSpace(line); k != "" || !ask {
			return k, nil
		}
		return "", errors.New("no API key: set CLOUDEX_API_KEY or pipe the key in")
	}
	if !ask {
		return "", nil
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
