package cli

import (
	"errors"
	"fmt"
	"net/url"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

func newDashboardCmd(opts *options) *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "dashboard [sandbox]",
		Short: "Open the deployment's web dashboard in your browser, signed in",
		Long: "Open the dashboard, which the deployment serves at its API URL: sandboxes with\n" +
			"their terminals, desktops, agents, commands and files, named saves, and (for\n" +
			"admins) tokens, sign-ins and provider keys. With a sandbox, it opens on that one.\n\n" +
			"It signs the browser in with a one-time link that lasts 60 seconds. The browser\n" +
			"gets a session of its own, with this computer's role, which lasts 30 days from\n" +
			"its last use and shows in cloudex tokens, where it can be revoked.",
		Example: "  cloudex dashboard\n  cloudex dashboard my-box\n  cloudex dashboard --no-open",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			path := "/"
			if len(args) == 1 {
				path = "/s/" + url.PathEscape(args[0])
			}
			link, err := client.LoginLink(cmd.Context(), path)
			if err != nil {
				var apiErr *api.Error
				if errors.As(err, &apiErr) && apiErr.Status == 404 {
					return errors.New("this deployment has no dashboard yet (it came in v0.2.1): update it with cloudex deploy")
				}
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), link)
			}
			if !noOpen {
				openBrowser(link.URL)
			}
			out := cmd.OutOrStdout()
			if noOpen {
				fmt.Fprintf(out, "Open this link within %d seconds; it signs a browser in once:\n%s\n", link.ExpiresIn, link.URL)
			} else {
				fmt.Fprintf(out, "Opening the dashboard. If no browser opens, open this link within %d seconds:\n%s\n", link.ExpiresIn, link.URL)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the link instead of opening a browser")
	return cmd
}
