package cli

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/lucataco/cloudex/go/internal/sshkey"
	"github.com/spf13/cobra"
)

// sshHostSuffix lets plain ssh, scp, rsync and editors reach sandboxes as
// <id or name>.cloudex once `cloudex ssh-config` is in ~/.ssh/config.
const sshHostSuffix = ".cloudex"

var sandboxIDRE = regexp.MustCompile(`^cx-[a-z0-9]{6}$`)

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
			"WebSocket, so the sandbox needs no public address. For a sandbox on a node this\n" +
			"computer reaches over the LAN or a tailnet, it goes straight to the node instead\n" +
			"(the direct path), with the API as the fallback; scp, rsync, ssh-config and attach\n" +
			"take the same path. Anything after -- goes to ssh, for example:\n" +
			"cloudex ssh my-box -- -L 3000:localhost:3000\n\n" +
			"Environment:\n" +
			"  CLOUDEX_DIRECT=auto|off|only  the direct path: tried first (auto, the default),\n" +
			"                                never (off), or required (only, for debugging)\n" +
			"  CLOUDEX_DEBUG=1               print which path each connection took, and why",
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
			if err := needRunning(s); err != nil {
				return err
			}
			notePlacement(client.BaseURL, *s)
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
			mode, err := directMode()
			if err != nil {
				return err
			}
			dir, err := config.Dir()
			if err != nil {
				return err
			}
			var key string
			if port == 22 {
				if _, key, err = sshkey.Ensure(dir); err != nil {
					return err
				}
			}
			ctx, cancel := context.WithCancel(cmd.Context())
			defer cancel()
			errOut := cmd.ErrOrStderr()
			var logf func(string, ...any)
			if debugOn() {
				logf = func(format string, a ...any) { fmt.Fprintf(errOut, "cloudex proxy: "+format+"\n", a...) }
			}
			cache := readDirectCache(dir, client.BaseURL)
			if mode == "auto" && cache.Older > time.Now().UnixMilli() {
				mode = "off" // the deployment has no /direct; it's asked again within the hour
			}

			var conn net.Conn
			if mode == "off" {
				// The Worker path only, as before v0.2.3.
				if port == 22 && !sandboxIDRE.MatchString(r) {
					s, err := client.Get(ctx, r)
					if err != nil {
						return err
					}
					trackName(errOut, dir, r, s.ID)
					r = s.ID
				}
				if logf != nil {
					logf("CLOUDEX_DIRECT=off or an older deployment: the Worker path")
				}
				if conn, err = dialWorker(ctx, client, r, port, key); err != nil {
					return err
				}
			} else {
				race := &directRace{
					ref: r, port: port, mode: mode,
					api:    func(c context.Context) (*api.Direct, error) { return client.Direct(c, r, port, key) },
					worker: func(c context.Context, ref string) (net.Conn, error) { return dialWorker(c, client, ref, port, key) },
					get:    func(c context.Context) (*api.Sandbox, error) { return client.Get(c, r) },
					nets:   readLocalNets(),
					cache:  cache,
					times:  defaultRaceTimes,
					logf:   logf,
				}
				if sandboxIDRE.MatchString(r) {
					race.id = r
				} else if port == 22 {
					race.id = readSSHNames(dir)[r] // the name's last id, for cached candidates only
				}
				race.late = func(f func(*directCache)) {
					updateDirectCache(dir, client.BaseURL, time.Now(), []func(*directCache){f})
				}
				if v := strings.TrimSpace(os.Getenv("CLOUDEX_DIRECT_CANDIDATES")); v != "" {
					race.override = strings.Split(v, ",")
				}
				out, err := race.run(ctx)
				updateDirectCache(dir, client.BaseURL, time.Now(), race.changes)
				if port == 22 && !sandboxIDRE.MatchString(r) && race.answered != "" {
					trackName(errOut, dir, r, race.answered)
				}
				if err != nil {
					return err
				}
				conn = out.conn
			}
			return splice(ctx, conn)
		},
	}
}

func trackName(errOut io.Writer, dir, name, id string) {
	if err := trackSSHName(dir, name, id); err != nil {
		fmt.Fprintf(errOut, "cloudex: couldn't record which sandbox %s%s is (%v). If ssh says its host key has changed, use cloudex ssh %s\n", name, sshHostSuffix, err, name)
	}
}

// dialWorker opens the Worker path: a WebSocket through the Sandbox object.
// The connection lives as long as ctx's parent, not the dial's ctx.
func dialWorker(ctx context.Context, client *api.Client, ref string, port int, key string) (net.Conn, error) {
	ws, err := client.DialTCP(ctx, ref, port, key)
	if err != nil {
		return nil, err
	}
	return &wsConn{Conn: websocket.NetConn(context.WithoutCancel(ctx), ws, websocket.MessageBinary)}, nil
}

// wsConn marks the Worker path, whose close statuses aren't errors.
type wsConn struct{ net.Conn }

// splice connects stdin and stdout to conn until either ends.
func splice(ctx context.Context, conn net.Conn) error {
	// ssh closes our stdin when it's done; closing the connection then
	// makes the read below fail, which isn't an error worth printing. The
	// direct path is TCP end to end, so there the end of stdin is passed
	// on as a half-close and the answer still comes back (cloudex proxy
	// box 8000 < request); ssh ends the proxy itself when it exits.
	var closed atomic.Bool
	go func() {
		io.Copy(conn, os.Stdin)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok && cw.CloseWrite() == nil {
			return
		}
		closed.Store(true)
		conn.Close()
	}()
	_, err := io.Copy(os.Stdout, conn)
	conn.Close()
	if err == nil || closed.Load() || ctx.Err() != nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	if _, ws := conn.(*wsConn); ws && websocket.CloseStatus(err) != -1 {
		return nil
	}
	return err
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

// ssh-config reaches a sandbox as <name>.cloudex, and OpenSSH keys known_hosts
// by that name. A name can move to another sandbox, with other host keys, so
// the proxy records which id each name had, and forgets the name's keys when
// it points somewhere new.
func sshNamesPath(dir string) string { return filepath.Join(dir, "ssh-names.json") }

func readSSHNames(dir string) map[string]string {
	names := map[string]string{}
	if data, err := os.ReadFile(sshNamesPath(dir)); err == nil {
		if json.Unmarshal(data, &names) != nil || names == nil {
			names = map[string]string{}
		}
	}
	return names
}

func writeSSHNames(dir string, names map[string]string) error {
	data, err := json.MarshalIndent(names, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(sshNamesPath(dir), append(data, '\n'), 0o600)
}

// writeFileAtomic writes through a temporary file of its own, so that proxies
// running at once don't write over each other's.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
}

func trackSSHName(dir, name, id string) error {
	names := readSSHNames(dir)
	if names[name] == id {
		return nil
	}
	if err := removeKnownHost(filepath.Join(dir, "known_hosts"), name+sshHostSuffix); err != nil {
		return err
	}
	names[name] = id
	return writeSSHNames(dir, names)
}

// Tidying only: the proxy forgets a stale name when it next sees it, so a
// failure is let go.
func forgetSSHName(name string) {
	dir, err := config.Dir()
	if err != nil {
		return
	}
	if removeKnownHost(filepath.Join(dir, "known_hosts"), name+sshHostSuffix) != nil {
		return
	}
	if names := readSSHNames(dir); names[name] != "" {
		delete(names, name)
		writeSSHNames(dir, names)
	}
}

// removeKnownHost deletes the lines of a known_hosts file that are for host,
// in any of the forms OpenSSH writes: plain, [host]:port, and hashed.
func removeKnownHost(path, host string) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var kept strings.Builder
	removed := false
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if isKnownHostLine(line, host) {
			removed = true
			continue
		}
		kept.WriteString(line)
	}
	if !removed {
		return nil
	}
	mode := fs.FileMode(0o600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return writeFileAtomic(path, []byte(kept.String()), mode)
}

func isKnownHostLine(line, host string) bool {
	fields := strings.Fields(line)
	if len(fields) > 0 && strings.HasPrefix(fields[0], "@") { // @cert-authority, @revoked
		fields = fields[1:]
	}
	if len(fields) < 3 || strings.HasPrefix(fields[0], "#") {
		return false
	}
	for _, h := range strings.Split(fields[0], ",") {
		if h == host || strings.HasPrefix(h, "["+host+"]:") || hashedHostIs(h, host) {
			return true
		}
	}
	return false
}

// hashedHostIs checks a |1|salt|hash entry, which ssh writes with HashKnownHosts.
func hashedHostIs(entry, host string) bool {
	parts := strings.Split(entry, "|")
	if len(parts) != 4 || parts[1] != "1" {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)) == parts[3]
}
