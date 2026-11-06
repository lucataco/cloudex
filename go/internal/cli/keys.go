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

// The providers, in the order help lists them, and the environment variable
// each one's key usually lives in on this computer.
var providerNames = []string{"anthropic", "openai", "openrouter", "google", "groq", "mistral", "xai", "deepseek"}

var providerEnv = map[string]string{
	"anthropic":  "ANTHROPIC_API_KEY",
	"openai":     "OPENAI_API_KEY",
	"openrouter": "OPENROUTER_API_KEY",
	"google":     "GEMINI_API_KEY",
	"groq":       "GROQ_API_KEY",
	"mistral":    "MISTRAL_API_KEY",
	"xai":        "XAI_API_KEY",
	"deepseek":   "DEEPSEEK_API_KEY",
}

var providerList = strings.Join(providerNames[:len(providerNames)-1], ", ") + " or " + providerNames[len(providerNames)-1]

func newKeysCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "keys",
		Short: "Give sandboxes AI provider keys without putting the keys in them",
		Long: "Store an AI provider's API key in your deployment: " + providerList + ".\n" +
			"Sandboxes started after that get a placeholder key, and the real one is added to\n" +
			"their HTTPS calls to the provider as they leave, so it never enters a sandbox, its\n" +
			"saves, backups or forks. Claude Code, Codex, OpenCode, the providers' SDKs and curl\n" +
			"work as if the key were there. A key a sandbox brings itself is used as is.\n" +
			"While a provider's key is stored here, the agents in a sandbox use it even if\n" +
			"someone signs in there with a subscription; to use a subscription instead,\n" +
			"remove the key with cloudex keys rm.\n\n" +
			"A new provider reaches a running sandbox when it next starts (stop it, then\n" +
			"resume it, to start it now); a changed key reaches it within a minute. With no\n" +
			"subcommand, lists the keys.",
		Example: "  cloudex keys set anthropic\n  cloudex keys set openai --from-env\n  cloudex keys set openrouter < key.txt\n" +
			"  cloudex keys\n  cloudex keys rm openai",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return listKeys(cmd, opts) },
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the stored keys (only their last 4 characters)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return listKeys(cmd, opts) },
	})

	var fromEnv bool
	set := &cobra.Command{
		Use:   "set <provider>",
		Short: "Store a provider's key",
		Long: "Store a provider's API key: " + providerList + ". It's read from a hidden\n" +
			"prompt, from standard input when piped, or with --from-env from the provider's\n" +
			"usual variable, such as $ANTHROPIC_API_KEY, $OPENROUTER_API_KEY or, for Google, $GEMINI_API_KEY.\n" +
			"The provider is asked whether it accepts the key first, and a key it rejects isn't stored.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := args[0]
			if _, ok := providerEnv[provider]; !ok {
				return fmt.Errorf("unknown provider %s: use %s", provider, providerList)
			}
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
			fmt.Fprintf(cmd.OutOrStdout(), "Stored the %s key (%s). Sandboxes started from now on use it.\n"+
				"A running sandbox that already uses an %s key gets this one within a minute; any other\n"+
				"gets it when it next starts. To use it now: cloudex stop <sandbox>, then cloudex resume <sandbox>.\n", k.Provider, k.Hint, k.Provider)
			return nil
		},
	}
	set.Flags().BoolVar(&fromEnv, "from-env", false, "read the key from the provider's usual variable, such as $ANTHROPIC_API_KEY")
	cmd.AddCommand(set)

	cmd.AddCommand(&cobra.Command{
		Use:     "rm <provider>",
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
		name := providerEnv[provider]
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
