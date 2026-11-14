package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/lucataco/cloudex/go/internal/node"
	"github.com/spf13/cobra"
)

// newForks lists this node's fork images (v0.2.3 M5) and, with --sweep,
// removes those no machine uses, as the agent does every hour: for the
// release checks (N70, N76) and for a host that needs the space now. Run in
// the agent's container, with its state volume.
func newForks(state func() node.State, socket *string) *cobra.Command {
	var sweep bool
	var age time.Duration
	cmd := &cobra.Command{
		Use:    "forks",
		Hidden: true,
		Short:  "List this node's fork images as JSON; --sweep removes the ones no machine uses",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := state()
			id, err := s.Identity()
			if err != nil || id == nil {
				return fmt.Errorf("not joined: %v", err)
			}
			d := &node.Docker{Socket: *socket}
			r := &node.Runtime{Docker: d, NodeID: id.ID, Forks: node.NewForkBook(s.Dir), Log: log.New(os.Stderr, "cloudex-node ", log.LstdFlags|log.Lmsgprefix)}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
			defer cancel()
			removed := 0
			if sweep {
				if age < 0 {
					return errors.New("--age can't be negative")
				}
				removed = r.SweepForks(ctx, age)
			}
			imgs, err := d.Images(ctx, node.LabelNode+"="+id.ID, node.LabelRole+"=fork")
			if err != nil {
				return err
			}
			type entry struct {
				Point  string `json:"point"`
				ID     string `json:"id"`
				ForkOf string `json:"forkOf"`
				node.ForkRecord
			}
			book := r.Forks.All()
			out := []entry{}
			for _, i := range imgs {
				p := i.Labels[node.LabelPoint]
				out = append(out, entry{Point: p, ID: i.ID, ForkOf: i.Labels[node.LabelForkOf], ForkRecord: book[p]})
			}
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{"images": out, "removed": removed})
		},
	}
	cmd.Flags().BoolVar(&sweep, "sweep", false, "remove fork images no machine uses")
	cmd.Flags().DurationVar(&age, "age", node.ForkSweepAge, "with --sweep, only those older than this")
	return cmd
}
