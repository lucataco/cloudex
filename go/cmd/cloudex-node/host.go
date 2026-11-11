package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/lucataco/cloudex/go/internal/node"
	"github.com/spf13/cobra"
)

// The commands the installer (scripts/node-install.sh) runs: the firewall's
// rules, a machine pool that collides with nothing, and, in the running
// agent's container, the self-test and a throwaway machine.

func newFirewall() *cobra.Command {
	var pool string
	var onlink, block []string
	cmd := &cobra.Command{
		Use:    "firewall",
		Hidden: true,
		Short:  "Print the nftables rules for machine networks (the installer writes them to /etc/cloudex-node/firewall.nft)",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			p, err := netip.ParsePrefix(pool)
			if err != nil {
				return fmt.Errorf("--pool: %w", err)
			}
			c := node.FirewallConfig{Pool: p}
			parse := func(list []string) ([]netip.Prefix, error) {
				var out []netip.Prefix
				for _, s := range list {
					for _, f := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
						q, err := netip.ParsePrefix(f)
						if err != nil {
							return nil, err
						}
						out = append(out, q)
					}
				}
				return out, nil
			}
			routes, err := parse(onlink)
			if err != nil {
				return fmt.Errorf("--onlink: %w", err)
			}
			c.OnLink = node.PublicOnLink(routes)
			if c.Block, err = parse(block); err != nil {
				return fmt.Errorf("--block: %w", err)
			}
			rules, err := node.FirewallRules(c)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), rules)
			return err
		},
	}
	cmd.Flags().StringVar(&pool, "pool", "", "the machine pool")
	cmd.Flags().StringArrayVar(&onlink, "onlink", nil, "the host's on-link prefixes (private ones are dropped, being blocked already)")
	cmd.Flags().StringArrayVar(&block, "block", nil, "more prefixes machines may not reach")
	return cmd
}

func newPickPool() *cobra.Command {
	var routes, socket string
	cmd := &cobra.Command{
		Use:    "pick-pool",
		Hidden: true,
		Short:  "Print a machine pool that overlaps no route of this network namespace and no Docker network (run with --network host)",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			used, err := node.HostRoutes(routes)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			ns, err := (&node.Docker{Socket: socket}).Networks(ctx)
			if err != nil {
				return err
			}
			for _, n := range ns {
				for _, c := range n.IPAM.Config {
					if p, err := netip.ParsePrefix(c.Subnet); err == nil {
						used = append(used, p)
					}
				}
			}
			p, err := node.PickPool(used)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), p)
			return nil
		},
	}
	cmd.Flags().StringVar(&routes, "routes", "/proc/net/route", "the routing table to read")
	cmd.Flags().StringVar(&socket, "docker-socket", node.DefaultDockerSocket, "the Docker Engine API's socket")
	return cmd
}

// newSelfTest runs, in the agent's container, the self-test or a throwaway
// machine (--machine), with the configuration the running agent saved.
func newSelfTest(state func() node.State, socket *string) *cobra.Command {
	var machine bool
	cmd := &cobra.Command{
		Use:    "selftest",
		Hidden: true,
		Short:  "Run the isolation self-test now and print its report as JSON (exits 1 when it fails); --machine boots and removes a throwaway machine instead",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := state()
			id, err := s.Identity()
			if err != nil || id == nil {
				return fmt.Errorf("not joined: %v", err)
			}
			rc, err := s.RuntimeConfig()
			if err != nil {
				return err
			}
			if rc == nil {
				return errors.New("the agent hasn't run with a machine pool yet (cloudex-node run --pool)")
			}
			lg := log.New(os.Stderr, "cloudex-node ", log.LstdFlags|log.Lmsgprefix)
			r, err := rc.Runtime(&node.Docker{Socket: *socket}, lg)
			if err != nil {
				return err
			}
			r.NodeID, r.Unisolated = id.ID, id.Unisolated
			if img := s.Image(); img != "" && r.ImageOverride == "" {
				r.SetImage(img)
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			if machine {
				// The agent's own self-test decides whether machines may run.
				st, _ := s.ReadStatus()
				if st != nil && st.Isolation != nil {
					r.Record(&node.SelfTestReport{Pass: st.Isolation.Firewall == "pass", Checks: []node.Check{{OK: true}}})
				}
				msg, err := r.MachineCheck(ctx)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), msg)
				return nil
			}
			rep := r.SelfTest(ctx)
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			enc.Encode(rep)
			if !rep.Pass {
				return exitCode(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&machine, "machine", false, "boot a throwaway machine, check it from inside, and remove it")
	return cmd
}
