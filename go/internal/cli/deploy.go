package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/lucataco/cloudex/go/internal/cloudflare"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/lucataco/cloudex/go/internal/deploy"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newDeployCmd(opts *options) *cobra.Command {
	var accountID, previewDomain, image string
	var skipCheck, yes bool
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Install or update Cloudex in your Cloudflare account",
		Long: "Install Cloudex in your Cloudflare account, or update it. It needs Workers Paid\n" +
			"($5/month) and an API token, read from $CLOUDFLARE_API_TOKEN or asked for.\n" +
			"Create the token with this link, which pre-fills the permissions:\n\n  " + deploy.TokenURL + "\n\n" +
			"Preview links need a domain in the same account: --preview-domain example.dev.\n" +
			"Running deploy again updates the API and image and keeps your sandboxes, your API\n" +
			"key and your preview domain. On another computer, run cloudex login instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			token, err := cloudflareToken(cmd)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "Deploying Cloudex:")
			ctx, cancel := context.WithTimeout(cmd.Context(), 45*time.Minute)
			defer cancel()
			res, err := deploy.Run(ctx, deploy.Options{
				Token: token, AccountID: firstNonEmpty(accountID, os.Getenv("CLOUDFLARE_ACCOUNT_ID")),
				PreviewDomain: previewDomain, PreviewDomainSet: cmd.Flags().Changed("preview-domain"),
				RememberedPreviewDomain: cfg.PreviewZone, Image: image,
				ExistingURL: cfg.APIURL, ExistingKey: cfg.APIKey,
				Confirm:   func(q string) bool { return yes || confirm(cmd, q) },
				SkipCheck: skipCheck, Log: cmd.ErrOrStderr(),
			})
			if res != nil {
				// Save even after a failed check, so status and doctor can see the deployment.
				if res.APIURL != cfg.APIURL {
					cfg.Current = ""
				}
				cfg.APIURL, cfg.APIKey, cfg.AccountID, cfg.PreviewZone = res.APIURL, res.APIKey, res.AccountID, res.PreviewZone
				if saveErr := cfg.Save(); saveErr != nil {
					return saveErr
				}
			}
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"apiUrl": res.APIURL, "accountId": res.AccountID, "previewZone": res.PreviewZone})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\nCloudex is running at %s.\nCreate a sandbox with: cloudex new\n", res.APIURL)
			return nil
		},
	}
	cmd.Flags().StringVar(&accountID, "account-id", "", "Cloudflare account id (needed when the token reaches several)")
	cmd.Flags().StringVar(&previewDomain, "preview-domain", "", "a domain in the account to serve preview links on ('' turns them off)")
	cmd.Flags().StringVar(&image, "image", deploy.DefaultImage, "sandbox image to install")
	cmd.Flags().BoolVar(&skipCheck, "skip-check", false, "don't start a test sandbox after deploying")
	cmd.Flags().BoolVar(&yes, "yes", false, "replace the API key of an existing deployment without asking")
	return cmd
}

// confirm asks whether to replace the key on the terminal; without one, the answer is no.
func confirm(cmd *cobra.Command, question string) bool {
	return ask(cmd, fmt.Sprintf("\n%s\n\nReplace the key? [y/N] ", question))
}

// ask puts a yes/no prompt on the terminal; without one, the answer is no.
func ask(cmd *cobra.Command, prompt string) bool {
	if !isTerminal(os.Stdin) {
		return false
	}
	fmt.Fprint(cmd.ErrOrStderr(), prompt)
	var answer string
	fmt.Fscanln(os.Stdin, &answer)
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes")
}

func cloudflareToken(cmd *cobra.Command) (string, error) {
	if t := strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN")); t != "" {
		return t, nil
	}
	if !isTerminal(os.Stdin) {
		return "", errors.New("set CLOUDFLARE_API_TOKEN to a Cloudflare API token; create one at " + deploy.TokenURL)
	}
	w := cmd.ErrOrStderr()
	fmt.Fprintf(w, "Cloudex needs a Cloudflare API token. Create one with the permissions pre-filled:\n\n  %s\n\nPaste the token (it isn't shown): ", deploy.TokenURL)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(w)
	if err != nil {
		return "", err
	}
	if t := strings.TrimSpace(string(b)); t != "" {
		return t, nil
	}
	return "", errors.New("no token given")
}

func newDoctorCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check this computer and your deployment for problems",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			problems := 0
			check := func(ok bool, good, bad string) {
				if ok {
					fmt.Fprintln(w, "ok   ", good)
				} else {
					problems++
					fmt.Fprintln(w, "fix  ", bad)
				}
			}
			_, err := exec.LookPath("ssh")
			check(err == nil, "ssh is installed", "install an OpenSSH client; cloudex ssh, scp and prompt use it")
			_, err = exec.LookPath("npx")
			check(err == nil, "Node.js is installed (cloudex deploy runs wrangler with it)", "install Node.js 20 or newer before running cloudex deploy")

			cfg, client, err := opts.load()
			check(err == nil, "a deployment is configured", "run cloudex deploy")
			if err == nil {
				version := "?"
				h, err := client.Health(cmd.Context())
				if err == nil {
					version = h.Version
				}
				check(err == nil, fmt.Sprintf("the API at %s answers (version %s)", client.BaseURL, version),
					fmt.Sprintf("the API at %s doesn't answer: %v", client.BaseURL, err))
				if err == nil {
					_, err := client.List(cmd.Context())
					check(err == nil, "the API key works", fmt.Sprintf("the API rejected the key (%v); run cloudex deploy again", err))
				}
			}
			if token := os.Getenv("CLOUDFLARE_API_TOKEN"); token != "" && cfg != nil && cfg.AccountID != "" {
				err := cloudflare.New(token).CheckContainers(cmd.Context(), cfg.AccountID)
				check(err == nil, "the account can run containers", fmt.Sprintf("containers: %v", err))
			}
			if problems > 0 {
				return exitError{1}
			}
			return nil
		},
	}
}
