package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/spf13/cobra"
)

const (
	desktopLocalPort = 6080
	desktopTimeout   = 11 * time.Minute // the API allows the first start 10
)

var screenSize = regexp.MustCompile(`^\d{3,4}x\d{3,4}$`)

func newDesktopCmd(opts *options) *cobra.Command {
	var size string
	var port, quality int
	var link, noOpen, stop bool
	cmd := &cobra.Command{
		Use:   "desktop [sandbox]",
		Short: "Open a sandbox's desktop, with Chrome and a terminal, in your browser",
		Long: "Show a desktop running in the sandbox in your browser. The first time installs\n" +
			"it, which takes a minute or two; after that it starts in seconds. Windows that\n" +
			"programs in the sandbox open, such as an agent's browser, appear on it.\n\n" +
			"By default the desktop is reached through a port on this computer until you\n" +
			"press Ctrl-C; it keeps running in the sandbox after that. With --link you get an\n" +
			"HTTPS link on your preview domain instead, which works from any browser while\n" +
			"the sandbox runs, for anyone who has it. The sandbox stays up while a browser\n" +
			"shows its desktop.",
		Example: "  cloudex desktop\n  cloudex desktop my-box --size 1920x1080\n  cloudex desktop --link\n  cloudex desktop --stop",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !screenSize.MatchString(size) {
				return fmt.Errorf("bad size %q: use WIDTHxHEIGHT, such as 1280x720", size)
			}
			if quality < 0 || quality > 9 {
				return fmt.Errorf("bad quality %d: use 0 (least bandwidth) to 9", quality)
			}
			cfg, client, err := opts.load()
			if err != nil {
				return err
			}
			r, err := ref(cfg, optionalRef(args))
			if err != nil {
				return err
			}
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer cancel()
			s, err := client.Get(ctx, r)
			if err != nil {
				return err
			}
			if s.State != "running" {
				return fmt.Errorf("%s is %s; start it with: cloudex resume %s", s.Label(), s.State, s.Label())
			}
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()

			if stop {
				if err := client.StopDesktop(ctx, s.ID); err != nil {
					return desktopError(err)
				}
				if opts.json {
					return writeJSON(out, map[string]bool{"stopped": true})
				}
				_, err := fmt.Fprintf(out, "Stopped the desktop on %s.\n", s.Label())
				return err
			}

			fmt.Fprintf(errOut, "Starting the desktop on %s (the first time installs it, which takes a minute or two)...\n", s.Label())
			startCtx, cancelStart := context.WithTimeout(ctx, desktopTimeout)
			d, err := client.Desktop(startCtx, s.ID, size)
			cancelStart()
			if err != nil {
				return desktopError(err)
			}
			if cmd.Flags().Changed("quality") {
				d.Path = withQuery(d.Path, "quality", strconv.Itoa(quality))
			}

			if link {
				base, err := client.Host(ctx, s.ID, d.Port)
				if err != nil {
					return err
				}
				url := base + "/" + d.Path
				if opts.json {
					return writeJSON(out, map[string]string{"url": url, "size": d.Size})
				}
				fmt.Fprintln(out, url)
				fmt.Fprintln(errOut, "Anyone with this link can use the desktop while the sandbox runs.")
				if !noOpen {
					openBrowser(url)
				}
				return nil
			}

			ln, err := listenLocal(port)
			if err != nil {
				return err
			}
			defer ln.Close()
			url := fmt.Sprintf("http://%s/%s", ln.Addr(), d.Path)
			if opts.json {
				if err := writeJSON(out, map[string]string{"url": url, "size": d.Size}); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(out, "The desktop of %s (%s) is at\n\n  %s\n\n", s.Label(), d.Size, url)
				fmt.Fprintln(errOut, "Press Ctrl-C to close the connection; the desktop keeps running in the sandbox.")
			}
			if !noOpen {
				openBrowser(url)
			}
			return forward(ctx, ln, client, s.ID, d.Port, errOut)
		},
	}
	cmd.Flags().StringVar(&size, "size", "1280x720", "screen size; a running desktop is resized")
	cmd.Flags().BoolVar(&link, "link", false, "print an HTTPS link on your preview domain instead of using a local port")
	cmd.Flags().IntVar(&port, "port", 0, fmt.Sprintf("local port to use (default %d, or a free one)", desktopLocalPort))
	cmd.Flags().IntVar(&quality, "quality", 3, "picture quality, 0 to 9: lower uses less bandwidth on moving images (text stays sharp)")
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "don't open a browser")
	cmd.Flags().BoolVar(&stop, "stop", false, "stop the desktop and the programs on it")
	return cmd
}

// withQuery sets one option in the viewer's path, keeping the others.
func withQuery(path, key, value string) string {
	page, query, _ := strings.Cut(path, "?")
	values, err := url.ParseQuery(query)
	if err != nil {
		return path
	}
	values.Set(key, value)
	return page + "?" + values.Encode()
}

// desktopError explains a 404 from a deployment that predates the desktop.
func desktopError(err error) error {
	var apiErr *api.Error
	if errors.As(err, &apiErr) && apiErr.Status == 404 && apiErr.Message == "not found" {
		return errors.New("this deployment doesn't have the desktop yet; update it with: cloudex deploy")
	}
	return err
}

// listenLocal listens on localhost: on port if given, else on the usual port
// when it's free, else on any free port.
func listenLocal(port int) (net.Listener, error) {
	if port != 0 {
		return net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	}
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", desktopLocalPort)); err == nil {
		return ln, nil
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// forward bridges every connection to ln to port in the sandbox, each over its
// own WebSocket, until ctx ends.
func forward(ctx context.Context, ln net.Listener, client *api.Client, id string, port int, errOut io.Writer) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	var mu sync.Mutex
	lastErr := ""
	for {
		local, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer local.Close()
			ws, err := client.DialTCP(ctx, id, port, "")
			if err != nil {
				// The viewer retries every second; say each new problem once.
				mu.Lock()
				if msg := err.Error(); msg != lastErr && ctx.Err() == nil {
					lastErr = msg
					fmt.Fprintln(errOut, "cloudex:", msg)
				}
				mu.Unlock()
				return
			}
			remote := websocket.NetConn(ctx, ws, websocket.MessageBinary)
			defer remote.Close()
			go func() {
				io.Copy(remote, local)
				remote.Close()
			}()
			io.Copy(local, remote)
		}()
	}
}

// openBrowser opens url in the default browser, if there is one to open.
var openBrowser = func(url string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", url)
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		// Without a display, xdg-open may start a text browser in this terminal.
		if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
			return
		}
		c = exec.Command("xdg-open", url)
	}
	if c.Start() == nil {
		go c.Wait()
	}
}
