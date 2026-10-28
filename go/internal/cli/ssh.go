package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/lucataco/cloudex/go/internal/sshkey"
	"github.com/spf13/cobra"
)

// sshHostSuffix lets plain ssh, scp, rsync and editors reach sandboxes as
// <id or name>.cloudex once `cloudex ssh-config` is in ~/.ssh/config.
const sshHostSuffix = ".cloudex"

func self() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}

// proxyCommand is run by OpenSSH through a shell, so the path is quoted.
func proxyCommand(target string) (string, error) {
	exe, err := self()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s proxy %s 22", shellQuote(exe), target), nil
}

// sshArgs are the options every connection uses: the CLI's key, its own
// known_hosts file and the WebSocket proxy.
func sshArgs(r string) ([]string, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	key, _, err := sshkey.Ensure(dir)
	if err != nil {
		return nil, err
	}
	proxy, err := proxyCommand(shellQuote(r))
	if err != nil {
		return nil, err
	}
	return []string{
		"-i", key,
		"-o", "IdentitiesOnly=yes",
		"-o", "ProxyCommand=" + proxy,
		"-o", "UserKnownHostsFile=" + filepath.Join(dir, "known_hosts"),
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", "HostKeyAlias=" + r + sshHostSuffix,
		"-o", "ServerAliveInterval=30",
		"-o", "LogLevel=ERROR",
	}, nil
}

// runSSH runs the system ssh against a sandbox and passes on its exit code.
func runSSH(cmd *cobra.Command, r string, extra []string) error {
	bin, err := exec.LookPath("ssh")
	if err != nil {
		return errors.New("ssh isn't installed or isn't on PATH")
	}
	args, err := sshArgs(r)
	if err != nil {
		return err
	}
	args = append(args, extra...)
	c := exec.CommandContext(cmd.Context(), bin, args...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exitError{exit.ExitCode()}
		}
		return err
	}
	return nil
}

func newSSHCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh [sandbox] [-- ssh arguments or a command]",
		Short: "Open a shell in a sandbox over SSH",
		Long: "Open a shell in a sandbox. The connection runs through your Cloudex API over a\n" +
			"WebSocket, so the sandbox needs no public address. Anything after -- goes to ssh,\n" +
			"for example: cloudex ssh my-box -- -L 3000:localhost:3000",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			named, extra := splitAtDash(cmd, args)
			if len(named) > 1 {
				return errors.New("name at most one sandbox; put ssh arguments after --")
			}
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, optionalRef(named))
			if err != nil {
				return err
			}
			s, err := client.Get(cmd.Context(), r)
			if err != nil {
				return err
			}
			if s.State != "running" {
				return fmt.Errorf("%s is %s; start it with: cloudex resume %s", s.Label(), s.State, s.Label())
			}
			return runSSH(cmd, s.ID, append([]string{"cloudex@" + s.ID + sshHostSuffix}, extra...))
		},
	}
}

// splitAtDash separates positional arguments from those after "--".
func splitAtDash(cmd *cobra.Command, args []string) (before, after []string) {
	if i := cmd.ArgsLenAtDash(); i >= 0 {
		return args[:i], args[i:]
	}
	return args, nil
}

func newProxyCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:    "proxy <sandbox> <port>",
		Short:  "Connect stdin and stdout to a TCP port in a sandbox (an ssh ProxyCommand)",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, client, err := opts.load()
			if err != nil {
				return err
			}
			r := strings.TrimSuffix(args[0], sshHostSuffix)
			port, err := strconv.Atoi(args[1])
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("bad port %q", args[1])
			}
			var key string
			if port == 22 {
				dir, err := config.Dir()
				if err != nil {
					return err
				}
				if _, key, err = sshkey.Ensure(dir); err != nil {
					return err
				}
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			conn, err := client.DialTCP(ctx, r, port, key)
			if err != nil {
				return err
			}
			nc := websocket.NetConn(ctx, conn, websocket.MessageBinary)
			go func() {
				io.Copy(nc, os.Stdin)
				nc.Close()
			}()
			_, err = io.Copy(os.Stdout, nc)
			if err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) && websocket.CloseStatus(err) == -1 {
				return err
			}
			return nil
		},
	}
}

func newSSHConfigCmd(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh-config",
		Short: "Print an ~/.ssh/config block so ssh, scp, rsync and editors reach <sandbox>.cloudex",
		Example: "  cloudex ssh-config >> ~/.ssh/config\n" +
			"  scp notes.txt my-box.cloudex:\n" +
			"  rsync -a ./app/ cx-7f3a2k.cloudex:app/",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			key, _, err := sshkey.Ensure(dir)
			if err != nil {
				return err
			}
			proxy, err := proxyCommand("%n")
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), `# Cloudex sandboxes: ssh <id or name>%s
Host *%s
  User cloudex
  ProxyCommand %s
  IdentityFile %s
  IdentitiesOnly yes
  UserKnownHostsFile %s
  StrictHostKeyChecking accept-new
  ServerAliveInterval 30
`, sshHostSuffix, sshHostSuffix, proxy, key, filepath.Join(dir, "known_hosts"))
			return err
		},
	}
}
