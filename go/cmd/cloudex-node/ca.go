package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/lucataco/cloudex/go/internal/node"
	"github.com/spf13/cobra"
)

// cloudex-node ca: the node CA (v0.2.2 M6), which machines trust for the
// AI providers' hosts only. show prints its certificate, never its key.
// test-cert (hidden) signs a certificate for any name with the CA's key, as a
// stolen key could, for the release checks to show that machines refuse it
// for every name but the providers'.
func newCA(state func() node.State) *cobra.Command {
	ca := &cobra.Command{Use: "ca", Short: "The node CA that machines trust for the AI providers' hosts"}
	show := &cobra.Command{
		Use:   "show",
		Short: "Print the node CA's certificate, its fingerprint and the hosts it is limited to",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := state().LoadCA()
			if err != nil {
				return err
			}
			if c == nil {
				return errors.New("this node has no CA yet: the agent makes one when the deployment names its providers")
			}
			fmt.Fprintf(cmd.OutOrStdout(), "# sha256 %s\n# hosts %s\n%s", c.Fingerprint(), strings.Join(c.Hosts, " "), c.PEM)
			return nil
		},
	}
	test := &cobra.Command{
		Use:    "test-cert <name>",
		Short:  "Sign a certificate for any name with the node CA (release checks only)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := state().LoadCA()
			if err != nil {
				return err
			}
			if c == nil {
				return errors.New("this node has no CA yet")
			}
			p, err := c.SignAnyForTest(args[0])
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(p)
			return err
		},
	}
	ca.AddCommand(show, test)
	return ca
}
