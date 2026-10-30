package cli

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

var (
	envNameRE     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	sandboxRefRE  = regexp.MustCompile(`^(cx-[a-z0-9]{6}|[a-z][a-z0-9-]{0,30}[a-z0-9])$`)
	settingsAfter = "New shells get the change at once; a stopped sandbox gets it when it next starts."
)

func newEnvCmd(opts *options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env [sandbox]",
		Short: "Set a sandbox's environment variables and files",
		Long: "A sandbox's settings: environment variables that every shell, ssh session,\n" +
			"exec and agent gets, and files in its home directory. They apply at every\n" +
			"start, and at once to a running sandbox; forks get a copy. The deployment keeps\n" +
			"them encrypted and never shows values or file contents again. With no\n" +
			"subcommand, lists the names, the files and the repo cloned at creation.\n\n" +
			"GH_TOKEN or GITHUB_TOKEN lets git in the sandbox clone and push private\n" +
			"GitHub repos over HTTPS.",
		Example: "  cloudex env\n  cloudex env set DATABASE_URL=postgres://... GH_TOKEN=$(gh auth token)\n" +
			"  cloudex env set web --env-file .env --file .npmrc=$HOME/.npmrc\n  cloudex env unset web DATABASE_URL --file .npmrc",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, optionalRef(args))
			if err != nil {
				return err
			}
			st, err := client.Settings(cmd.Context(), r)
			if err != nil {
				return err
			}
			return printSettings(cmd, opts, st)
		},
	}

	var envFiles, files []string
	set := &cobra.Command{
		Use:   "set [sandbox] NAME=VALUE...",
		Short: "Set environment variables, and files with --file",
		Long: "Set environment variables, from NAME=VALUE arguments and --env-file, and files\n" +
			"with --file DEST=SRC, where DEST is a path in the sandbox's home directory and\n" +
			"SRC a file on this computer. " + settingsAfter,
		RunE: func(cmd *cobra.Command, args []string) error {
			named := ""
			if len(args) > 0 && !strings.Contains(args[0], "=") {
				named, args = args[0], args[1:]
			}
			env, err := envSettings(args, envFiles)
			if err != nil {
				return err
			}
			fileMap, err := fileSettings(files)
			if err != nil {
				return err
			}
			if len(env) == 0 && len(fileMap) == 0 {
				return errors.New("nothing to set: give NAME=VALUE, --env-file or --file")
			}
			ch := api.SettingsChange{Env: map[string]*string{}, Files: map[string]*string{}}
			for k, v := range env {
				ch.Env[k] = &v
			}
			for k, v := range fileMap {
				ch.Files[k] = &v
			}
			return updateSettings(cmd, opts, named, ch)
		},
	}
	set.Flags().StringArrayVar(&envFiles, "env-file", nil, "read NAME=VALUE lines from this file (repeatable)")
	set.Flags().StringArrayVar(&files, "file", nil, "DEST=SRC: put the local file SRC at DEST in the sandbox's home (repeatable)")
	cmd.AddCommand(set)

	var unsetFiles []string
	unset := &cobra.Command{
		Use:     "unset [sandbox] NAME...",
		Aliases: []string{"rm"},
		Short:   "Remove environment variables, and files with --file",
		Long: "Remove environment variables, and with --file, files the settings put in the\n" +
			"home directory. With two or more arguments, a first one that could be a\n" +
			"sandbox is taken as one. " + settingsAfter,
		RunE: func(cmd *cobra.Command, args []string) error {
			named := ""
			if len(args) > 1 && sandboxRefRE.MatchString(args[0]) {
				named, args = args[0], args[1:]
			}
			if len(args) == 0 && len(unsetFiles) == 0 {
				return errors.New("nothing to remove: give variable names or --file")
			}
			ch := api.SettingsChange{Env: map[string]*string{}, Files: map[string]*string{}}
			for _, name := range args {
				ch.Env[name] = nil
			}
			for _, f := range unsetFiles {
				ch.Files[sandboxPath(f)] = nil
			}
			return updateSettings(cmd, opts, named, ch)
		},
	}
	unset.Flags().StringArrayVar(&unsetFiles, "file", nil, "a file in the sandbox's home to remove (repeatable)")
	cmd.AddCommand(unset)
	return cmd
}

func updateSettings(cmd *cobra.Command, opts *options, named string, ch api.SettingsChange) error {
	cfg, client, err := opts.load()
	if err != nil {
		return err
	}
	r, err := ref(cfg, named)
	if err != nil {
		return err
	}
	st, err := client.UpdateSettings(cmd.Context(), r, ch)
	if err != nil {
		return err
	}
	if !opts.json {
		fmt.Fprintln(cmd.ErrOrStderr(), settingsAfter)
	}
	return printSettings(cmd, opts, st)
}

func printSettings(cmd *cobra.Command, opts *options, st *api.Settings) error {
	out := cmd.OutOrStdout()
	if opts.json {
		return writeJSON(out, st)
	}
	list := func(items []string) string {
		if len(items) == 0 {
			return "none"
		}
		return strings.Join(items, " ")
	}
	fmt.Fprintf(out, "Variables  %s\n", list(st.Env))
	fmt.Fprintf(out, "Files      %s\n", list(st.Files))
	if st.Repo != nil {
		branch := ""
		if st.Repo.Branch != nil {
			branch = " (" + *st.Repo.Branch + ")"
		}
		fmt.Fprintf(out, "Repo       %s%s in %s\n", st.Repo.URL, branch, st.Repo.Dir)
	}
	return nil
}

// envSettings reads NAME=VALUE arguments and .env files; arguments win.
func envSettings(pairs, envFiles []string) (map[string]string, error) {
	env := map[string]string{}
	for _, path := range envFiles {
		if err := readEnvFile(path, env); err != nil {
			return nil, err
		}
	}
	for _, p := range pairs {
		name, value, ok := strings.Cut(p, "=")
		if !ok || !envNameRE.MatchString(name) {
			return nil, fmt.Errorf("%q isn't NAME=VALUE", p)
		}
		env[name] = value
	}
	return env, nil
}

// readEnvFile reads a .env file: NAME=VALUE lines, optionally after "export",
// with # comment lines; quotes around a whole value are removed.
func readEnvFile(path string, env map[string]string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		name = strings.TrimSpace(name)
		if !ok || !envNameRE.MatchString(name) {
			return fmt.Errorf("%s:%d isn't NAME=VALUE", path, n)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		env[name] = value
	}
	return sc.Err()
}

// fileSettings reads DEST=SRC pairs: SRC on this computer, DEST in the sandbox's home.
func fileSettings(specs []string) (map[string]string, error) {
	files := map[string]string{}
	for _, s := range specs {
		dest, src, ok := strings.Cut(s, "=")
		if !ok || dest == "" || src == "" {
			return nil, fmt.Errorf("--file %q isn't DEST=SRC", s)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return nil, err
		}
		files[sandboxPath(dest)] = string(data)
	}
	return files, nil
}

// sandboxPath turns a destination the shell may have expanded (~/.npmrc became
// /Users/me/.npmrc) back into one in the sandbox's home.
func sandboxPath(dest string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(dest, home+"/") {
		return "~/" + strings.TrimPrefix(dest, home+"/")
	}
	return dest
}
