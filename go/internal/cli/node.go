package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

func newNodeCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "node",
		Aliases: []string{"nodes"},
		Short:   "Run sandboxes on your own Docker hosts: add, list and remove nodes",
		Long: "A node is a Linux host of your own, with Docker, that runs sandboxes for this\n" +
			"deployment. An agent on it, cloudex-node, dials out to the deployment and stays\n" +
			"connected; the host needs no open port. cloudex node add prints the command that\n" +
			"installs the agent with a join token, which works once, for 30 minutes.\n" +
			"Adding and removing nodes needs the admin key or an admin token.\n" +
			"With no subcommand, lists the nodes.",
		Example: "  cloudex node add box\n  cloudex node list\n  cloudex node info box\n  cloudex node rm box",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return listNodes(cmd, opts) },
	}
	cmd.AddCommand(&cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the nodes, whether each is online, and what it has",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return listNodes(cmd, opts) },
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "add <name>",
		Short: "Add a node, and print the command that installs its agent",
		Long: "Add a node and print the command to run on it as root, which starts the agent\n" +
			"with a join token. The token works once, for 30 minutes; adding the same name\n" +
			"again before the node joins gives a new one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			n, err := client.CreateNode(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), n)
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Added node %s (%s). On it, as root, with Docker running, run:\n\n  %s\n\n", n.Node.Name, n.Node.ID, n.Install)
			fmt.Fprintf(w, "Or run the agent as a plain process:\n\n  cloudex-node run --join %s --api %s --state-dir <a directory of its own>\n\n", n.JoinToken, client.BaseURL)
			fmt.Fprintf(w, "The token works once, until %s. Then cloudex node list shows %s online.\n", time.UnixMilli(n.ExpiresAt).Format("15:04"), n.Node.Name)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "info <name>",
		Short: "Show a node: its state, its host, where it connects, and a round trip to it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			n, err := client.GetNode(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), n)
			}
			return printNode(cmd, n)
		},
	})

	var force, yes bool
	rm := &cobra.Command{
		Use:     "rm <name>",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a node: its agent is disconnected and its key stops working",
		Long: "Remove a node. Its agent is disconnected at once and can't connect again; the\n" +
			"node stays listed as revoked until it's removed a second time. A node that still\n" +
			"has sandboxes is refused unless --force is given.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			if !yes {
				if !isTerminal(os.Stdin) {
					return errors.New("removing a node can't be undone; pass --yes to confirm")
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Remove node %s? Its agent is disconnected and must join again with a new token. [y/N] ", args[0])
				answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if a := strings.ToLower(strings.TrimSpace(answer)); a != "y" && a != "yes" {
					return errors.New("not removed")
				}
			}
			out, err := client.DeleteNode(cmd.Context(), args[0], force)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), out)
			}
			if _, ok := out["revoked"]; ok {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Removed %s: its agent is disconnected and can't connect again. cloudex node rm %s again forgets it.\n", args[0], args[0])
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "Forgot %s.\n", args[0])
			}
			return err
		},
	}
	rm.Flags().BoolVar(&force, "force", false, "remove it even though it has sandboxes")
	rm.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask for confirmation")
	cmd.AddCommand(rm)
	return cmd
}

func listNodes(cmd *cobra.Command, opts *options) error {
	_, client, err := opts.load()
	if err != nil {
		return err
	}
	ns, err := client.ListNodes(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if opts.json {
		if ns == nil {
			ns = []api.Node{}
		}
		return writeJSON(out, ns)
	}
	if len(ns) == 0 {
		_, err := fmt.Fprintln(out, "No nodes. Add your own Docker host with: cloudex node add <name>")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tRUNTIME\tISOLATION\tCPUS\tMEMORY\tDISK FREE\tMACHINES\tSEEN")
	for _, n := range ns {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", clean(n.Name), n.State, orDash(n.Runtime), isolationLabel(&n), intOrDash(n.CPUs),
			memory(n.MemoryMB), disk(n.DiskFreeGB), n.Machines, seen(&n))
	}
	return tw.Flush()
}

func printNode(cmd *cobra.Command, n *api.Node) error {
	tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	row("Name", clean(n.Name))
	row("ID", n.ID)
	row("State", n.State)
	row("Last seen", seen(n))
	row("Agent", orDash(n.Version))
	row("Host", fmt.Sprintf("%s, %s CPUs, %s memory, %s disk free", orDash(n.Arch), intOrDash(n.CPUs), memory(n.MemoryMB), disk(n.DiskFreeGB)))
	row("Runtime", orDash(n.Runtime))
	row("Isolation", isolationLabel(n))
	where := orDash(n.Colo)
	if n.LocationHint != nil {
		where += ", sandboxes in " + *n.LocationHint
	}
	row("Connects to", where)
	if n.RTTMs != nil {
		row("Round trip", fmt.Sprintf("%d ms", *n.RTTMs))
	}
	row("Sandboxes", fmt.Sprint(n.Machines))
	row("Added", ago(n.CreatedAt))
	return tw.Flush()
}

// isolationLabel says how machines on the node are kept apart, as far as M2
// knows: the firewall self-test comes with the Docker runtime.
func isolationLabel(n *api.Node) string {
	switch {
	case n.Isolation == nil:
		return "-"
	case n.Isolation.Unisolated:
		return "none (testing)"
	case n.Isolation.Firewall == "pass":
		return orDash(n.Runtime)
	case n.Isolation.Firewall == "fail":
		return "failing"
	default:
		return "unchecked"
	}
}

func seen(n *api.Node) string {
	switch {
	case n.State == "online":
		return "now"
	case n.LastSeenAt == nil:
		return "never"
	default:
		return ago(*n.LastSeenAt)
	}
}

func orDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return clean(*s)
}

func intOrDash(v *int) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprint(*v)
}

func memory(mb *int) string {
	if mb == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f GiB", float64(*mb)/1024)
}

func disk(gb *float64) string {
	if gb == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f GB", *gb)
}

// clean keeps what a node reported from moving the terminal's cursor or
// changing its colors: the API checks it, and this doesn't trust that.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return '?'
		}
		return r
	}, s)
}
