package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type health struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

func newStatusCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Check that the Cloudex API is reachable",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := fetchHealth(cmd.Context(), opts.apiURL)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"apiUrl": opts.apiURL, "ok": h.OK, "apiVersion": h.Version})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "API at %s is up (version %s)\n", opts.apiURL, h.Version)
			return err
		},
	}
}

func fetchHealth(ctx context.Context, apiURL string) (*health, error) {
	if apiURL == "" {
		return nil, errors.New("no API URL: pass --api-url or set CLOUDEX_API_URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(apiURL, "/")+"/v1/health", nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API unreachable: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned %s", res.Status)
	}
	var h health
	if err := json.NewDecoder(res.Body).Decode(&h); err != nil {
		return nil, fmt.Errorf("unexpected health response: %w", err)
	}
	if !h.OK {
		return nil, errors.New("API reports it is not healthy")
	}
	return &h, nil
}
