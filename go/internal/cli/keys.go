package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// The environment variable each provider's key usually lives in on this computer.
var providerEnv = map[string]string{"anthropic": "ANTHROPIC_API_KEY", "openai": "OPENAI_API_KEY"}

func newKeysCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Give sandboxes AI provider keys without putting the keys in them",
		Long: "Store an Anthropic or OpenAI API key in your deployment. Sandboxes started after\n" +
			"that get a placeholder key, and the real one is added to their HTTPS calls to\n" +
			"the provider as they leave, so it never enters a sandbox, its saves, backups or\n" +
			"forks. Claude Code, Codex, the providers' SDKs and curl work as if the key were\n" +
			"there. A key a sandbox brings itself, or a subscription login, is used as is.\n\n" +
			"A new provider reaches a running sandbox when it next starts; a changed key\n" +
			"reaches it within a minute. With no subcommand, lists the keys.",
		Example: "  cloudex keys set anthropic\n  cloudex keys set openai --from-env\n  cloudex keys\n  cloudex keys rm openai",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return listKeys(cmd, opts) },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the stored keys (only their last 4 characters)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return listKeys(cmd, opts) },
	})

	var fromEnv bool
	set := &cobra.Command{
		Use:   "set <anthropic|openai>",
		Short: "Store a provider's key",
		Long: "Store a provider's API key. It's read from a hidden prompt, from standard input\n" +
			"when piped, or with --from-env from $ANTHROPIC_API_KEY or $OPENAI_API_KEY.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := args[0]
			key, err := providerKey(cmd, provider, fromEnv)
			if err != nil {
				return err
			}
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			k, err := client.SetKey(cmd.Context(), provider, key)
			if err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), k)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Stored the %s key (%s). Sandboxes started from now on use it; running ones\n"+
				"get it when they next start, or within a minute if they already use a %s key.\n", k.Provider, k.Hint, k.Provider)
			return nil
		},
	}
	set.Flags().BoolVar(&fromEnv, "from-env", false, "read the key from $ANTHROPIC_API_KEY or $OPENAI_API_KEY")
	cmd.AddCommand(set)

	cmd.AddCommand(&cobra.Command{
		Use:     "rm <anthropic|openai>",
		Aliases: []string{"delete"},
		Short:   "Remove a provider's key",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			if err := client.DeleteKey(cmd.Context(), args[0]); err != nil {
				return err
			}
			if opts.json {
				return writeJSON(cmd.OutOrStdout(), map[string]string{"deleted": args[0]})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed the %s key. Sandboxes that used it lose it within a minute.\n", args[0])
			return nil
		},
	})
	return cmd
}

func listKeys(cmd *cobra.Command, opts *options) error {
	_, client, err := opts.load()
	if err != nil {
		return err
	}
	keys, err := client.ListKeys(cmd.Context())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if opts.json {
		if keys == nil {
			return writeJSON(out, []any{})
		}
		return writeJSON(out, keys)
	}
	if len(keys) == 0 {
		_, err := fmt.Fprintln(out, "No provider keys. Add one with: cloudex keys set anthropic")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tKEY\tUPDATED")
	for _, k := range keys {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", k.Provider, k.Hint, ago(k.UpdatedAt))
	}
	return tw.Flush()
}

// providerKey reads a key from this computer's environment, piped standard
// input or a hidden prompt.
func providerKey(cmd *cobra.Command, provider string, fromEnv bool) (string, error) {
	if fromEnv {
		name, ok := providerEnv[provider]
		if !ok {
			return "", fmt.Errorf("unknown provider %s: use anthropic or openai", provider)
		}
		if k := strings.TrimSpace(os.Getenv(name)); k != "" {
			return k, nil
		}
		return "", fmt.Errorf("$%s isn't set", name)
	}
	if !isTerminal(os.Stdin) {
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if k := strings.TrimSpace(line); k != "" {
			return k, nil
		}
		return "", errors.New("no key: pipe it in, or use --from-env")
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%s API key (it isn't shown): ", provider)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", err
	}
	if k := strings.TrimSpace(string(b)); k != "" {
		return k, nil
	}
	return "", errors.New("no key given")
}
