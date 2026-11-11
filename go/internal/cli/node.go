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
			fmt.Fprintf(w, "The token works once, until %s. The installer sets up a firewall for the node's\n", time.UnixMilli(n.ExpiresAt).Format("15:04"))
			fmt.Fprintf(w, "machines, starts the agent, and checks the isolation; then cloudex node list shows\n%s online.\n\n", n.Node.Name)
			fmt.Fprintln(w, plainDockerWarning(n.Node.Name))
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
			"has sandboxes is refused unless --force is given. With --force, an online node\n" +
			"removes their machines first; the sandboxes stay listed, in state error, until\n" +
			"you delete them.",
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
	if n.Isolation != nil && n.Isolation.CheckedAt != nil {
		row("Self-test", ago(*n.Isolation.CheckedAt))
	}
	if n.Isolation != nil {
		for _, f := range n.Isolation.Failed {
			row("  failed", clean(f))
		}
	}
	if n.Image != nil {
		img := clean(n.Image.Ref) + ", " + n.Image.State
		if n.Image.Progress != nil && n.Image.State == "pulling" {
			img += fmt.Sprintf(" (%.0f%%)", *n.Image.Progress*100)
		}
		row("Image", img)
	}
	where := orDash(n.Colo)
	if n.LocationHint != nil {
		where += ", sandboxes in " + *n.LocationHint
	}
	row("Connects to", where)
	if n.RTTMs != nil {
		row("Round trip", fmt.Sprintf("%d ms", *n.RTTMs))
	}
	row("Sandboxes", fmt.Sprint(n.Machines))
	if n.ClockSkewSec != nil && (*n.ClockSkewSec > 5 || *n.ClockSkewSec < -5) {
		row("Clock", skewText(*n.ClockSkewSec))
	}
	if len(n.PendingDeletes) > 0 {
		row("Pending deletes", fmt.Sprintf("%s (deleted while it was offline; it removes their machines when it's back)", clean(strings.Join(n.PendingDeletes, " "))))
	}
	if len(n.Orphans) > 0 {
		row("Orphans", fmt.Sprintf("%s (machines of no sandbox, left alone; remove them on the host with docker rm -f cloudex-<id>)", clean(strings.Join(n.Orphans, " "))))
	}
	row("Added", ago(n.CreatedAt))
	return tw.Flush()
}

// isolationLabel says how machines on the node are kept apart: by the
// runtime, when the agent's firewall self-test passes ("docker" is plain
// Docker, for trusted code only); none on a node that joined with
// --unisolated, for testing.
func isolationLabel(n *api.Node) string {
	switch {
	case n.Isolation == nil:
		return "-"
	case n.Isolation.Unisolated:
		return "none (testing)"
	case n.Isolation.Firewall == "pass" && n.Runtime != nil && *n.Runtime == "docker":
		return "docker (trusted code only)"
	case n.Isolation.Firewall == "pass":
		return orDash(n.Runtime)
	case n.Isolation.Firewall == "fail":
		return "failing (no machines)"
	default:
		return "unchecked"
	}
}

func skewText(sec int) string {
	if sec > 0 {
		return fmt.Sprintf("%d s ahead of the deployment's", sec)
	}
	return fmt.Sprintf("%d s behind the deployment's", -sec)
}

// plainDockerWarning is what node add says, and what new --on says on a
// plain-Docker node (v0.2.2 M5).
func plainDockerWarning(node string) string {
	return "If the host has no Sysbox, machines on " + node + " run under plain Docker. Root in a machine\n" +
		"is root on " + node + " if code escapes the container, and from there its LAN and tailnet\n" +
		"are in reach. Use such a node only for code you trust. With Sysbox installed before the\n" +
		"agent, root in a machine is an unprivileged user on the host."
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
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return '?'
		}
		return r
	}, s)
}
