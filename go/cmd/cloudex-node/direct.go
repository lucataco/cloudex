package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/direct"
	"github.com/lucataco/cloudex/go/internal/node"
	"github.com/spf13/cobra"
)

// The direct path's commands (v0.2.3): the sidecar's relay, the installer's
// choice of addresses, and the release checks' probe.

// directConfig checks run's --direct-port and --direct-addr.
func directConfig(port int, addrs []string) (node.DirectConfig, error) {
	c := node.DirectConfig{Port: port}
	if port < 1 || port > 65535 {
		return c, fmt.Errorf("--direct-port %d isn't a port", port)
	}
	for _, s := range addrs {
		for _, f := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
			a, err := netip.ParseAddr(f)
			if err != nil || !direct.Usable(a) {
				return c, fmt.Errorf("--direct-addr %s: give a private (RFC 1918) or tailnet (100.64/10) IPv4 address", f)
			}
			c.Addrs = append(c.Addrs, a.Unmap())
		}
	}
	if len(c.Addrs) == 0 {
		return c, errors.New("--direct-port needs at least one --direct-addr")
	}
	if len(c.Addrs) > node.DirectAddrsMax {
		return c, fmt.Errorf("at most %d --direct-addr", node.DirectAddrsMax)
	}
	return c, nil
}

func newRelay() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:    "relay",
		Hidden: true,
		Short:  "Pass connections on the direct port to the agent (the sidecar " + node.RelayName + " runs this)",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ap, err := netip.ParseAddrPort(to)
			if err != nil || !ap.Addr().Is4() {
				return fmt.Errorf("--to: give the agent's IPv4 address and port")
			}
			lg := log.New(os.Stderr, "cloudex-node relay ", log.LstdFlags|log.Lmsgprefix)
			ln, err := net.Listen("tcp4", fmt.Sprintf(":%d", node.RelayPort))
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			lg.Printf("%s: relaying %s to the agent at %s", version, ln.Addr(), ap)
			return node.Relay(ctx, ln, ap.String(), lg)
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the agent's control address and port")
	return cmd
}

func newDirectAddrs() *cobra.Command {
	return &cobra.Command{
		Use:    "direct-addrs",
		Hidden: true,
		Short:  "Print the host's private and tailnet IPv4 addresses for the direct port, one a line (run with --network host)",
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ifs, err := node.HostIfaces()
			if err != nil {
				return err
			}
			// The addresses on stdout, for the installer; what each is on
			// stderr, which it shows (review D-L1).
			for _, a := range node.ChooseDirectAddrsWhy(ifs) {
				fmt.Fprintln(cmd.OutOrStdout(), a.Addr)
				fmt.Fprintf(cmd.ErrOrStderr(), "  %s on %s (%s)\n", a.Addr, a.Iface, a.Kind)
			}
			return nil
		},
	}
}

// newProbeDirect is the direct port's check (plan M2, N60): with tickets
// from the deployment (CLOUDEX_API_URL and CLOUDEX_API_KEY, as the CLI
// reads them), it tries the port with faults, and prints one line per case:
// its name, PASS or FAIL, and what the node did. With --pipe, it opens one
// good session and carries stdin and stdout over it (an ssh ProxyCommand).
func newProbeDirect() *cobra.Command {
	var addr, sandbox, sshKey, cases, apiURL string
	var port, flood int
	var pipe bool
	var save, present string
	cmd := &cobra.Command{
		Use:   "direct",
		Short: "Try the direct port at --addr with good and bad tickets for --sandbox, and print what it did (exit 1 when a case fails); --pipe opens one session on stdin/stdout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			apiURL = firstNonEmpty(apiURL, os.Getenv("CLOUDEX_API_URL"))
			key := os.Getenv("CLOUDEX_API_KEY")
			if apiURL == "" || key == "" || sandbox == "" || addr == "" {
				return errors.New("give --addr and --sandbox, and CLOUDEX_API_URL and CLOUDEX_API_KEY in the environment")
			}
			c := api.New(apiURL, key)
			p := &directProbe{c: c, addr: addr, sandbox: sandbox, port: port, out: cmd.OutOrStdout()}
			if sshKey != "" {
				b, err := os.ReadFile(sshKey)
				if err != nil {
					return err
				}
				p.sshKey = strings.TrimSpace(string(b))
			}
			ctx := cmd.Context()
			if save != "" {
				// A ticket to present later (after the sandbox stops, say).
				_, _, d, err := p.ticket(ctx)
				if err != nil {
					return err
				}
				b, _ := json.Marshal(d)
				return os.WriteFile(save, b, 0o600)
			}
			if present != "" {
				b, err := os.ReadFile(present)
				if err != nil {
					return err
				}
				var d api.Direct
				if err := json.Unmarshal(b, &d); err != nil || d.Ticket == nil {
					return fmt.Errorf("%s: not a saved answer with a ticket", present)
				}
				pub, _ := base64.StdEncoding.DecodeString(d.PublicKey)
				fmt.Fprintln(cmd.OutOrStdout(), p.line(ctx, pub, "CXD1 "+*d.Ticket+"\n"))
				return nil
			}
			if pipe {
				return p.pipe(ctx, cmd.InOrStdin(), cmd.OutOrStdout())
			}
			if flood > 0 {
				return p.flood(ctx, flood)
			}
			return p.run(ctx, strings.Split(cases, ","))
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "the direct port: host:port")
	cmd.Flags().StringVar(&sandbox, "sandbox", "", "a running sandbox on the node")
	cmd.Flags().IntVar(&port, "port", 22, "the sandbox's port")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "an ssh public key file to authorize in the sandbox (the ticket's key=)")
	cmd.Flags().StringVar(&apiURL, "api", "", "the deployment (default $CLOUDEX_API_URL)")
	cmd.Flags().StringVar(&cases, "cases", "good,replay,altered,othersandbox,otherport,othernode,noticket,garbage,long,plaintext,noalpn,wrongpin,expired", "the cases, comma-separated")
	cmd.Flags().BoolVar(&pipe, "pipe", false, "open one session and carry stdin and stdout over it")
	cmd.Flags().StringVar(&save, "save", "", "instead: save a fresh ticket (the API's answer) to this file")
	cmd.Flags().StringVar(&present, "present", "", "instead: present the ticket saved in this file once, and print the answer")
	cmd.Flags().IntVar(&flood, "flood", 0, "instead: open this many TCP connections at once that send nothing (a slow-loris), and say how the node closed them; then a good session")
	return cmd
}

type directProbe struct {
	c       *api.Client
	addr    string
	sandbox string
	port    int
	sshKey  string
	out     io.Writer
	mu      sync.Mutex
}

func (p *directProbe) printf(format string, args ...any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	fmt.Fprintf(p.out, format, args...)
}

// ticket gets a fresh ticket and the node's key.
func (p *directProbe) ticket(ctx context.Context) (string, ed25519.PublicKey, *api.Direct, error) {
	d, err := p.c.Direct(ctx, p.sandbox, p.port, p.sshKey)
	if err != nil {
		return "", nil, nil, err
	}
	if d.Provider != "node" || d.Ticket == nil || *d.Ticket == "" {
		return "", nil, d, fmt.Errorf("no direct ticket for %s (provider %s, direct %v)", p.sandbox, d.Provider, d.Direct != nil)
	}
	pub, err := base64.StdEncoding.DecodeString(d.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", nil, d, errors.New("the API's publicKey isn't an Ed25519 key")
	}
	return *d.Ticket, ed25519.PublicKey(pub), d, nil
}

func (p *directProbe) dialTLS(ctx context.Context, cfg *tls.Config) (*tls.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return nil, err
	}
	c := tls.Client(raw, cfg)
	if err := c.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return c, nil
}

// line sends a line after TLS and reads the answer.
func (p *directProbe) line(ctx context.Context, pub ed25519.PublicKey, send string) string {
	c, err := p.dialTLS(ctx, direct.PinnedTLS(pub))
	if err != nil {
		return "tls: " + err.Error()
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(8 * time.Second))
	if _, err := io.WriteString(c, send); err != nil {
		return "write: " + err.Error()
	}
	l, err := direct.ReadLine(c, 512)
	if err != nil {
		return "closed: " + err.Error()
	}
	if l == "ok" {
		// What the machine's port says first (sshd's banner).
		banner, _ := direct.ReadLine(c, 512)
		return "ok " + strings.TrimSpace(banner)
	}
	return l
}

func flipLast(s string) string {
	b := []byte(s)
	if b[len(b)-1] == 'A' {
		b[len(b)-1] = 'B'
	} else {
		b[len(b)-1] = 'A'
	}
	return string(b)
}

// reclaim makes a ticket whose claims say something else, under the
// original MAC (so the MAC fails), or under a secret of its own.
func reclaim(t string, change func(*direct.Claims), secret []byte) string {
	payload, mac, _ := strings.Cut(t, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	var c direct.Claims
	json.Unmarshal(raw, &c)
	change(&c)
	if secret != nil {
		return direct.Mint(secret, c)
	}
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b) + "." + mac
}

func (p *directProbe) run(ctx context.Context, cases []string) error {
	failed := 0
	check := func(name, got string, ok bool) {
		v := "PASS"
		if !ok {
			v = "FAIL"
			failed++
		}
		p.printf("%-13s %s %s\n", name, v, got)
	}
	isErr := func(got string) bool { return strings.HasPrefix(got, "err ") }
	var used string
	var usedPub ed25519.PublicKey
	for _, name := range cases {
		t, pub, _, err := p.ticket(ctx)
		if err != nil {
			return err
		}
		switch name {
		case "good":
			got := p.line(ctx, pub, "CXD1 "+t+"\n")
			check(name, got, strings.HasPrefix(got, "ok "))
			used, usedPub = t, pub
		case "replay":
			if used == "" {
				p.line(ctx, pub, "CXD1 "+t+"\n")
				used, usedPub = t, pub
			}
			got := p.line(ctx, usedPub, "CXD1 "+used+"\n")
			check(name, got, got == "err used already")
		case "altered":
			got := p.line(ctx, pub, "CXD1 "+flipLast(t)+"\n")
			check(name, got, isErr(got))
		case "othersandbox":
			got := p.line(ctx, pub, "CXD1 "+reclaim(t, func(c *direct.Claims) { c.Sandbox = "cx-zzzzzz" }, nil)+"\n")
			check(name, got, isErr(got))
		case "otherport":
			got := p.line(ctx, pub, "CXD1 "+reclaim(t, func(c *direct.Claims) { c.Port = 2222 }, nil)+"\n")
			check(name, got, isErr(got))
		case "othernode":
			// Claims for another node, under a secret that isn't this node's
			// (as an impostor's deployment would mint them).
			secret := make([]byte, 32)
			copy(secret, "an impostor's secret, not this.")
			got := p.line(ctx, pub, "CXD1 "+reclaim(t, func(c *direct.Claims) { c.Node = "nd-impostor" }, secret)+"\n")
			check(name, got, isErr(got))
		case "noticket":
			got := p.line(ctx, pub, "CXD1 \n")
			check(name, got, isErr(got))
		case "garbage":
			got := p.line(ctx, pub, "SSH-2.0-OpenSSH_9.6\r\n")
			check(name, got, isErr(got))
		case "long":
			got := p.line(ctx, pub, "CXD1 "+strings.Repeat("A", 2000)+"\n")
			check(name, got, isErr(got) || strings.HasPrefix(got, "closed"))
		case "plaintext":
			// The ticket without TLS: the node must not take it.
			got := func() string {
				raw, err := net.DialTimeout("tcp", p.addr, 5*time.Second)
				if err != nil {
					return "dial: " + err.Error()
				}
				defer raw.Close()
				raw.SetDeadline(time.Now().Add(8 * time.Second))
				io.WriteString(raw, "CXD1 "+t+"\n")
				b := make([]byte, 64)
				n, err := raw.Read(b)
				if n > 0 && strings.HasPrefix(string(b[:n]), "ok") {
					return "ok (no TLS!)"
				}
				return fmt.Sprintf("closed after %d bytes (%v)", n, err)
			}()
			check(name, got, !strings.HasPrefix(got, "ok"))
		case "noalpn":
			cfg := direct.PinnedTLS(pub)
			cfg.NextProtos = nil
			cfg.VerifyConnection = nil
			got := func() string {
				c, err := p.dialTLS(ctx, cfg)
				if err != nil {
					return "tls: " + err.Error()
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(8 * time.Second))
				io.WriteString(c, "CXD1 "+t+"\n")
				l, err := direct.ReadLine(c, 512)
				if err != nil {
					return "closed: " + err.Error()
				}
				return l
			}()
			check(name, got, !strings.HasPrefix(got, "ok"))
		case "wrongpin":
			other, _, _ := ed25519.GenerateKey(nil)
			got := p.line(ctx, other, "CXD1 "+t+"\n")
			check(name, got, strings.Contains(got, direct.ErrPin.Error()))
		case "expired":
			wait := direct.TicketTTL + node.DirectGrace + 2*time.Second
			p.printf("%-13s waiting %v for the ticket to expire\n", name, wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			got := p.line(ctx, pub, "CXD1 "+t+"\n")
			check(name, got, got == "err expired")
		default:
			return fmt.Errorf("no case %q", name)
		}
	}
	if failed > 0 {
		return exitCode(1)
	}
	return nil
}

// pipe opens one session and carries stdin and stdout over it.
func (p *directProbe) pipe(ctx context.Context, in io.Reader, out io.Writer) error {
	t, pub, _, err := p.ticket(ctx)
	if err != nil {
		return err
	}
	hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(hctx, "tcp", p.addr)
	if err != nil {
		return err
	}
	c, err := direct.Handshake(hctx, raw, pub, t)
	if err != nil {
		return err
	}
	defer c.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(c, in); c.CloseWrite(); done <- struct{}{} }()
	go func() { io.Copy(out, c); done <- struct{}{} }()
	<-done
	return nil
}

// flood opens n connections at once that send nothing, and says how soon
// the node closed each; then a good session must still work once the
// deadline has passed.
func (p *directProbe) flood(ctx context.Context, n int) error {
	var at sync.WaitGroup
	var fast, deadline, open, failed atomic.Int64
	t0 := time.Now()
	for i := 0; i < n; i++ {
		at.Add(1)
		go func() {
			defer at.Done()
			c, err := net.DialTimeout("tcp", p.addr, 5*time.Second)
			if err != nil {
				failed.Add(1)
				return
			}
			defer c.Close()
			s := time.Now()
			c.SetReadDeadline(time.Now().Add(15 * time.Second))
			b := make([]byte, 1)
			c.Read(b)
			switch d := time.Since(s); {
			case d < time.Second:
				fast.Add(1)
			case d < 10*time.Second:
				deadline.Add(1)
			default:
				open.Add(1)
			}
		}()
	}
	at.Wait()
	p.printf("flood %d: closed within 1 s %d, at the deadline (1-10 s) %d, still open after 15 s %d, dial failed %d, in %v\n",
		n, fast.Load(), deadline.Load(), open.Load(), failed.Load(), time.Since(t0).Round(time.Millisecond))
	t, pub, _, err := p.ticket(ctx)
	if err != nil {
		return err
	}
	got := p.line(ctx, pub, "CXD1 "+t+"\n")
	p.printf("after the flood: %s\n", got)
	if !strings.HasPrefix(got, "ok ") || open.Load() > 0 {
		return exitCode(1)
	}
	return nil
}
