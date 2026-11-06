package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

func newTokensCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "Give CI, agents and other computers their own API keys, and approve sign-ins",
		Long: "Access tokens work in place of the deployment's admin key, each with a role,\n" +
			"and can be limited to the sandboxes they make, expire, and be revoked at once:\n\n" +
			"  admin   everything, including tokens and provider keys\n" +
			"  user    everything to sandboxes and saves, but not tokens or provider keys\n" +
			"  read    lists, info, agent events, command output and file reads\n\n" +
			"--own limits a token to the sandboxes it makes (and their forks): it sees no\n" +
			"others, and can start from named saves but not make or delete them. That suits\n" +
			"CI and agents. Making and revoking tokens needs the admin key or an admin token.\n\n" +
			"cloudex login on another computer, without a key, asks for a token with a code;\n" +
			"approve it here with cloudex tokens approve. With no subcommand, lists the tokens.",
		Example: "  cloudex tokens create ci --own --expires 90d\n  cloudex tokens\n" +
			"  cloudex tokens approve BCDF-GHJK\n  cloudex tokens revoke tk-a2b3c4d5",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return listTokens(cmd, opts) },
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the tokens, with their roles and when each was last used",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return listTokens(cmd, opts) },
	})

	var role, expires string
	var own bool
	create := &cobra.Command{
		Use:   "create <name>",
		Short: "Make a token; its secret is shown once",
		Long: "Make an access token and print its secret, which nothing shows again. Give it to\n" +
			"the program that will use it as CLOUDEX_API_KEY, with CLOUDEX_API_URL set to this\n" +
			"deployment's URL, or with cloudex login on another computer.",
		Example: "  cloudex tokens create ci --own --expires 90d\n  cloudex tokens create dashboard-viewer --role read",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sec, err := expiry(expires)
			if err != nil {
				return err
			}
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			t, err := client.CreateToken(cmd.Context(), api.CreateTokenRequest{Name: args[0], Role: role, OwnOnly: own, ExpiresInSec: sec})
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), t)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Made %s (%s), %s. Its secret, which won't be shown again:\n\n  %s\n\n", t.ID, t.Name, describeToken(t.Token), t.Secret)
			fmt.Fprintf(w, "Use it with:\n\n  CLOUDEX_API_URL=%s CLOUDEX_API_KEY=<the secret> cloudex ...\n", cfg.APIURL)
			return nil
		},
	}
	create.Flags().StringVar(&role, "role", "user", "admin, user or read")
	create.Flags().BoolVar(&own, "own", false, "only the sandboxes this token makes")
	create.Flags().StringVar(&expires, "expires", "never", "how long it works, such as 12h, 30d or never")
	cmd.AddCommand(create)

	cmd.AddCommand(&cobra.Command{
		Use:     "revoke <id>",
		Aliases: []string{"rm"},
		Short:   "Revoke a token or a dashboard session; it stops working at once",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			if err := client.RevokeToken(cmd.Context(), args[0]); err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"revoked": args[0]})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s. It no longer works.\n", args[0])
			return err
		},
	})

	var arole, aexpires string
	var aown, yes bool
	approve := &cobra.Command{
		Use:   "approve [code]",
		Short: "Approve a computer's sign-in by the code it shows",
		Long: "cloudex login on a computer without a key shows a code such as BCDF-GHJK.\n" +
			"Approving it gives that computer a token of its own, which expires in 90 days\n" +
			"unless --expires says otherwise; the role is the one it asked for, or user.\n" +
			"With no code, lists the sign-ins waiting. Only approve a code you started\n" +
			"yourself: whoever holds the other computer gets the token.",
		Example: "  cloudex tokens approve BCDF-GHJK\n  cloudex tokens approve BCDF-GHJK --own --expires 7d",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			waiting, err := client.ListDeviceLogins(cmd.Context())
			if err != nil {
				return err
			}
			if len(args) == 0 {
				return listDeviceLogins(cmd, opts, waiting)
			}
			code := normalizeCode(args[0])
			var req *api.DeviceRequest
			for i := range waiting {
				if waiting[i].UserCode == code {
					req = &waiting[i]
				}
			}
			if req == nil {
				return fmt.Errorf("no sign-in is waiting with the code %s; it may have expired (codes last 10 minutes). cloudex tokens approve lists the ones waiting", code)
			}
			sec, err := expiry(aexpires)
			if err != nil {
				return err
			}
			if !yes && !opts.json {
				if !isTerminal(os.Stdin) {
					return errors.New("pass --yes to approve without a terminal to confirm on")
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Sign-in %s: %s, from %s, %s.\n", code, req.Name, where(req), ago(req.CreatedAt))
				if !ask(cmd, "Approve it? [y/N] ") {
					return errors.New("not approved")
				}
			}
			d, err := client.ApproveDeviceLogin(cmd.Context(), api.DeviceApproveRequest{UserCode: code, Role: arole, OwnOnly: aown, ExpiresInSec: sec})
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), d)
			}
			r := "user"
			if d.Role != nil {
				r = *d.Role
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Approved %s (%s, role %s). It gets its token within a few seconds.\n", code, d.Name, r)
			return err
		},
	}
	approve.Flags().StringVar(&arole, "role", "", "admin, user or read (default: the role it asked for, or user)")
	approve.Flags().BoolVar(&aown, "own", false, "only the sandboxes that computer makes")
	approve.Flags().StringVar(&aexpires, "expires", "90d", "how long its token works, such as 12h, 30d or never")
	approve.Flags().BoolVarP(&yes, "yes", "y", false, "approve without asking")
	cmd.AddCommand(approve)

	cmd.AddCommand(&cobra.Command{
		Use:   "deny <code>",
		Short: "Refuse a computer's sign-in",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			d, err := client.DenyDeviceLogin(cmd.Context(), normalizeCode(args[0]))
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), d)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Refused %s (%s).\n", d.UserCode, d.Name)
			return err
		},
	})
	return cmd
}

func listTokens(cmd *cobra.Command, opts *options) error {
	_, client, err := opts.load()
	if err != nil {
		return err
	}
	ts, err := client.ListTokens(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if opts.json {
		if ts == nil {
			ts = []api.Token{}
		}
		return writeJSON(out, ts)
	}
	if len(ts) == 0 {
		_, err := fmt.Fprintln(out, "No tokens. Make one with: cloudex tokens create <name>")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tNAME\tROLE\tEXPIRES\tLAST USED\tMADE")
	for _, t := range ts {
		role := t.Role
		if t.OwnOnly {
			role += ", own"
		}
		if t.Kind == "session" {
			role += " (dashboard)"
		}
		used := "never"
		if t.LastUsedAt != nil {
			used = ago(*t.LastUsedAt)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, role, until(t.ExpiresAt), used, ago(t.CreatedAt))
	}
	return tw.Flush()
}

func listDeviceLogins(cmd *cobra.Command, opts *options, rs []api.DeviceRequest) error {
	out := cmd.OutOrStdout()
	if opts.json {
		if rs == nil {
			rs = []api.DeviceRequest{}
		}
		return writeJSON(out, rs)
	}
	if len(rs) == 0 {
		_, err := fmt.Fprintln(out, "No sign-ins are waiting. On the other computer, run: cloudex login --url <this deployment's URL>")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "CODE\tNAME\tFROM\tASKED")
	for _, r := range rs {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.UserCode, r.Name, where(&r), ago(r.CreatedAt))
	}
	return tw.Flush()
}

func where(r *api.DeviceRequest) string {
	parts := []string{}
	if r.IP != nil {
		parts = append(parts, *r.IP)
	}
	if r.Country != nil {
		parts = append(parts, *r.Country)
	}
	if len(parts) == 0 {
		return "an unknown address"
	}
	return strings.Join(parts, ", ")
}

func describeToken(t api.Token) string {
	s := "role " + t.Role
	if t.OwnOnly {
		s += ", only the sandboxes it makes"
	}
	if t.ExpiresAt == nil {
		return s + ", no expiry"
	}
	return s + ", expires " + time.UnixMilli(*t.ExpiresAt).Format("2006-01-02 15:04")
}

func until(ms *int64) string {
	if ms == nil {
		return "never"
	}
	d := time.Until(time.UnixMilli(*ms))
	switch {
	case d <= 0:
		return "expired"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", int(d.Minutes())+1)
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh", int(d.Hours()))
	default:
		return fmt.Sprintf("in %dd", int(d.Hours()/24))
	}
}

// expiry reads 90d, 12h, 30m or never as seconds; 0 means never.
func expiry(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" || s == "never" || s == "0" {
		return 0, nil
	}
	var d time.Duration
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil {
			return 0, fmt.Errorf("can't read --expires %s: give a time such as 12h, 30d or never", s)
		}
		d = time.Duration(days) * 24 * time.Hour
	} else {
		var err error
		if d, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("can't read --expires %s: give a time such as 12h, 30d or never", s)
		}
	}
	if d < time.Minute || d > 10*365*24*time.Hour {
		return 0, fmt.Errorf("--expires %s: give between a minute and 10 years, or never", s)
	}
	return int64(d / time.Second), nil
}

// normalizeCode makes bcdf ghjk, BCDFGHJK and BCDF-GHJK the same code.
func normalizeCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	c := b.String()
	if len(c) == 8 {
		return c[:4] + "-" + c[4:]
	}
	return c
}
