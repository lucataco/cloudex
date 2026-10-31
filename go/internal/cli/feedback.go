package cli

// cloudex feedback: a bug report, an idea or a question for the Cloudex
// maintainers, as a public GitHub issue, from people and from their agents.
//
// The report is scrubbed of the deployment's key and addresses, link tokens,
// sandbox names, IP addresses and anything that looks like a secret, then
// shown. In a terminal it asks before sending. Without one (an agent), it saves
// the report as a draft and prints it with an id; `--send <id>` then sends
// exactly that text, so what goes out is what the person saw. It's filed with
// gh when gh is signed in, under that account, or as GitHub's new-issue page
// with the report filled in. A command that fails leaves its error behind for
// --last-error, with only the shape of the command: values stay local.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/lucataco/cloudex/go/internal/api"
	"github.com/lucataco/cloudex/go/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// feedbackMark starts every report, so the repo's workflow can label it and
// the triage skill find it.
const feedbackMark = "<!-- cloudex feedback v1 -->"

// feedbackRepo is where reports go; CLOUDEX_FEEDBACK_REPO points a fork's CLI elsewhere.
func feedbackRepo() string {
	return firstNonEmpty(os.Getenv("CLOUDEX_FEEDBACK_REPO"), "lucataco/cloudex")
}

// GitHub turns new-issue links much longer than this away.
const maxIssueURL = 7000

// A draft waits this long for --send.
const draftLifetime = 24 * time.Hour

// gh runs the GitHub CLI with stdin and returns its standard output, giving
// up after a while (gh can wait on the network or a prompt).
var gh = func(stdin string, args ...string) (string, error) {
	if _, err := exec.LookPath("gh"); err != nil {
		return "", err
	}
	limit := 90 * time.Second
	if len(args) > 0 && args[0] == "api" {
		limit = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()
	c := exec.CommandContext(ctx, "gh", args...)
	c.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	c.Stdout, c.Stderr = &out, &errOut
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errOut.String()))
	}
	return out.String(), nil
}

// lastError is what a failed command leaves in the config directory.
type lastError struct {
	Command string `json:"command"` // the command and its flag names; values left out
	Error   string `json:"error"`
	Version string `json:"version"`
	At      int64  `json:"at"`
	Crash   string `json:"crash,omitempty"` // the stack, when the CLI itself crashed
	// What the command was given (sandbox refs, names, URLs, domains). It stays
	// in this file, and is removed from the error before a report.
	Private []string `json:"private,omitempty"`
}

// privateValues is an error that knows values a report must not show, such as
// the account names a deploy lists.
type privateValues interface{ PrivateValues() []string }

func configFile(name string) (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func writeConfigJSON(name string, v any) {
	p, err := configFile(name)
	if err != nil {
		return
	}
	if data, err := json.MarshalIndent(v, "", "  "); err == nil {
		os.WriteFile(p, append(data, '\n'), 0o600)
	}
}

// usageError is a mistake in how a command was typed, which isn't worth
// reporting and mustn't replace the error that is.
func usageError(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	for _, p := range []string{"unknown command", "unknown flag", "unknown shorthand flag", "flag needs an argument", "invalid argument", "accepts ", "requires at least", "requires at most", "bad flag syntax"} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// saveLastError keeps a failed command for cloudex feedback --last-error.
// ran is the command cobra ran, nil when the CLI crashed.
func saveLastError(version string, ran *cobra.Command, args []string, err error, message, crash string) {
	if crash == "" && usageError(err) {
		return
	}
	line, private := describeCommand(ran, args)
	private = append(private, rawValues(args)...)
	var pv privateValues
	if errors.As(err, &pv) {
		private = append(private, pv.PrivateValues()...)
	}
	// The deployment it talked to, which may not be the one in use later.
	cfg, cfgErr := config.Load()
	if cfgErr != nil {
		cfg = &config.Config{}
	}
	apiURL := ""
	if ran != nil {
		if f := ran.Flags().Lookup("api-url"); f != nil {
			apiURL = f.Value.String()
		}
	}
	for _, u := range []string{apiURL, os.Getenv("CLOUDEX_API_URL"), cfg.APIURL} {
		for _, v := range urlValues(u, "") {
			private = append(private, v.value)
		}
	}
	private = append(private, os.Getenv("CLOUDEX_API_KEY"), cfg.APIKey, cfg.AccountID, os.Getenv("CLOUDFLARE_ACCOUNT_ID"))
	// The words the report shows anyway (the command, flag names, --size's
	// value) aren't private.
	shown := map[string]bool{}
	for _, w := range strings.Fields(line) {
		shown[w] = true
	}
	kept := private[:0]
	for _, v := range private {
		if v != "" && !shown[v] {
			kept = append(kept, v)
		}
	}
	private = kept
	writeConfigJSON("last-error.json", lastError{
		Command: line, Error: message, Version: version, At: time.Now().UnixMilli(), Crash: crash, Private: private,
	})
}

func loadLastError() (*lastError, error) {
	p, err := configFile("last-error.json")
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, errors.New("no failed command to report: cloudex keeps the last error of a command that fails")
	}
	var e lastError
	return &e, json.Unmarshal(data, &e)
}

// rawValues is everything a command line held that could be private, as typed:
// what follows = in an argument, a value stuck to a short flag (-pS3cret), and
// every other argument but the flags' own names, ports and counts.
func rawValues(args []string) []string {
	var out []string
	for _, a := range args {
		switch {
		case strings.Contains(a, "="):
			_, v, _ := strings.Cut(a, "=")
			out = append(out, v, a)
		case strings.HasPrefix(a, "--"):
		case strings.HasPrefix(a, "-") && len(a) > 2:
			out = append(out, a[2:], a)
		case strings.Trim(a, "0123456789") == "":
		default:
			out = append(out, a)
		}
	}
	return out
}

// Flag values that say how, not what, and can go in a report as they are.
var shownFlagValues = map[string]bool{"size": true, "agent": true, "count": true, "idle": true, "timeout": true, "quality": true}

// describeCommand gives a failed command as a report may show it, the command
// and its flag names, and the values it was given, which it mustn't.
func describeCommand(ran *cobra.Command, args []string) (string, []string) {
	var private []string
	keep := func(v string) {
		// Ports and counts are shown, so they aren't private.
		if v != "" && strings.Trim(v, "0123456789") != "" {
			private = append(private, v)
			if _, after, ok := strings.Cut(v, "="); ok && after != "" {
				private = append(private, after)
			}
		}
	}
	if ran == nil || !ran.HasParent() {
		// A crash, or a command cobra didn't find: the first word only.
		parts := []string{"cloudex"}
		for _, a := range args {
			keep(a)
			if !strings.HasPrefix(a, "-") && len(parts) == 1 {
				parts = append(parts, a)
			}
		}
		if len(args) > len(parts)-1 {
			parts = append(parts, "…")
		}
		return strings.Join(parts, " "), private
	}
	parts := []string{ran.CommandPath()}
	ran.Flags().Visit(func(f *pflag.Flag) {
		if f.Name == "json" {
			return
		}
		switch {
		case f.Value.Type() == "bool":
			parts = append(parts, "--"+f.Name)
		case shownFlagValues[f.Name]:
			parts = append(parts, "--"+f.Name, f.Value.String())
		default:
			parts = append(parts, "--"+f.Name, "…")
			if s, ok := f.Value.(pflag.SliceValue); ok {
				for _, v := range s.GetSlice() {
					keep(v)
				}
			} else {
				keep(f.Value.String())
			}
		}
	})
	for _, a := range ran.Flags().Args() {
		keep(a)
		if a != "" && strings.Trim(a, "0123456789") == "" {
			parts = append(parts, a) // a port or a count
		} else {
			parts = append(parts, "[arg]")
		}
	}
	return strings.Join(parts, " "), private
}

// looksLikeABug says whether a failure is worth suggesting a report for: the
// API failing on its own side. (Crashes say so themselves.)
func looksLikeABug(err error) bool {
	var apiErr *api.Error
	return errors.As(err, &apiErr) && apiErr.Status >= 500
}

var (
	previewHost  = regexp.MustCompile(`(?i)(\d{1,5})-(cx-[a-z0-9]{6})-[a-z0-9]{10}\b`)
	workersDev   = regexp.MustCompile(`(?i)\b(?:[a-z0-9-]+\.)?[a-z0-9-]+\.workers\.dev\b`)
	emailAddress = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}`)
	privateKey   = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?(-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
	urlUserinfo  = regexp.MustCompile(`(?i)\b([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`)
	knownSecret  = regexp.MustCompile(`\b(sk-ant-[A-Za-z0-9_-]{8,}|sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{16,}|[sr]k_(?:live|test)_[A-Za-z0-9]{16,}|AKIA[0-9A-Z]{16}|xox[abprs]-[A-Za-z0-9-]{10,}|cxk_[0-9a-f]{8,})`)
	// NAME=value and "name": "value" for names that hold secrets, however
	// they're joined (GH_TOKEN, DB_PASSWORD, clientSecret, PGPASSWORD,
	// STRIPE_KEY), and --password value or -pvalue.
	assignedSecret = regexp.MustCompile(`(?i)(^|[^A-Za-z0-9])([A-Za-z0-9_-]*?(?:api[_-]?key|key|token|secret|passw(?:or)?d|pwd|credentials?|auth)(?:[_-]?(?:key|token|secret|id))?["']?\s*[=:]\s*["']?)[^\s"',;}]{4,}`)
	flagSecret     = regexp.MustCompile(`(?i)(--?(?:password|passwd|token|secret|api-key|key)[ =]+)\S{4,}`)
	shortPassword  = regexp.MustCompile(`(^|\s)(-p)([A-Za-z][^\s]{3,})`)
	authHeader     = regexp.MustCompile(`(?i)\b(bearer|basic|token)(\s+)[A-Za-z0-9._~+/=-]{8,}`)
	longToken      = regexp.MustCompile(`\b[A-Za-z0-9_-]{32,}\b`)
	base64Run      = regexp.MustCompile(`[A-Za-z0-9+/]{32,}={0,2}`)
	ipv4           = regexp.MustCompile(`\b(?:25[0-5]|2[0-4]\d|1?\d?\d)(?:\.(?:25[0-5]|2[0-4]\d|1?\d?\d)){3}\b`)
	ipv6           = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4}\b`)
)

// privateIP is an address that says nothing about who sent a report: loopback,
// private ranges, and the sandbox's own.
func privateIP(ip string) bool {
	for _, p := range []string{"127.", "10.", "192.168.", "169.254.", "0.0.0.0"} {
		if strings.HasPrefix(ip, p) {
			return true
		}
	}
	var a, b int
	if n, _ := fmt.Sscanf(ip, "%d.%d.", &a, &b); n == 2 && a == 172 && b >= 16 && b <= 31 {
		return true
	}
	l := strings.ToLower(ip)
	return l == "::1" || l == "::" || strings.HasPrefix(l, "fd") || strings.HasPrefix(l, "fe80:")
}

// redactor removes what a public report mustn't hold.
type redactor struct {
	exact []*regexp.Regexp // case-insensitive, longest value first
	label []string
	names []*regexp.Regexp // sandbox names and refs, in text Cloudex made
	// Names that can't be an ordinary word (a dash, a digit, or 8 letters or
	// more), which also go from what people write.
	distinct []*regexp.Regexp
}

type exactValue struct{ value, label string }

func newRedactor(values []exactValue, sandboxNames []string) *redactor {
	seen := map[string]bool{}
	var list []exactValue
	for _, v := range values {
		k := strings.ToLower(v.value)
		if len(v.value) >= 3 && !seen[k] {
			seen[k] = true
			list = append(list, v)
		}
	}
	// Longest first, so a URL goes before the shorter values inside it.
	sort.SliceStable(list, func(i, j int) bool { return len(list[i].value) > len(list[j].value) })
	r := &redactor{}
	for _, v := range list {
		r.exact = append(r.exact, wholeValue(v.value))
		r.label = append(r.label, v.label)
	}
	names := map[string]bool{}
	for _, n := range sandboxNames {
		if len(n) < 2 || names[strings.ToLower(n)] || strings.HasPrefix(n, "-") {
			continue
		}
		names[strings.ToLower(n)] = true
		re := wholeValue(n)
		r.names = append(r.names, re)
		if len(n) >= 8 || strings.ContainsAny(n, "-0123456789") {
			r.distinct = append(r.distinct, re)
		}
	}
	return r
}

// wholeValue matches v in any case, and not inside a longer word.
func wholeValue(v string) *regexp.Regexp {
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
	re := regexp.QuoteMeta(v)
	if rs := []rune(v); word(rs[0]) {
		re = `\b` + re
	}
	if rs := []rune(v); word(rs[len(rs)-1]) {
		re += `\b`
	}
	return regexp.MustCompile(`(?i)` + re)
}

// identityValues are this computer's user name and host name.
func identityValues() []exactValue {
	var out []exactValue
	if u, err := user.Current(); err == nil && len(u.Username) >= 3 {
		out = append(out, exactValue{u.Username, "[user]"})
	}
	if h, err := os.Hostname(); err == nil && len(h) >= 3 {
		out = append(out, exactValue{h, "[host]"}, exactValue{strings.TrimSuffix(h, ".local"), "[host]"})
	}
	return out
}

// urlValues gives a URL and its host as values to remove.
func urlValues(raw, label string) []exactValue {
	if raw == "" {
		return nil
	}
	out := []exactValue{{raw, label}}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		out = append(out, exactValue{u.Host, "[API host]"})
		if h := u.Hostname(); h != u.Host {
			out = append(out, exactValue{h, "[API host]"})
		}
	}
	return out
}

// clean scrubs text a person wrote: secrets, addresses and the like.
func (r *redactor) clean(s string) string {
	// Shapes first, while a link or a key is still whole; then the values
	// this computer knows.
	s = privateKey.ReplaceAllString(s, "[private key]")
	s = urlUserinfo.ReplaceAllString(s, "${1}[credentials]@")
	s = previewHost.ReplaceAllString(s, "$1-$2-[token]")
	s = workersDev.ReplaceAllString(s, "[subdomain].workers.dev")
	s = emailAddress.ReplaceAllString(s, "[email]")
	s = knownSecret.ReplaceAllString(s, "[secret]")
	s = assignedSecret.ReplaceAllString(s, "${1}${2}[secret]")
	s = flagSecret.ReplaceAllString(s, "${1}[secret]")
	s = shortPassword.ReplaceAllString(s, "${1}${2}[secret]")
	s = authHeader.ReplaceAllString(s, "${1}${2}[secret]")
	s = longToken.ReplaceAllString(s, "[secret]")
	s = base64Run.ReplaceAllStringFunc(s, func(t string) string {
		// A secret mixes cases and digits; a path is mostly lowercase.
		if strings.ContainsAny(t, "0123456789") && strings.IndexFunc(t, unicode.IsUpper) >= 0 && strings.IndexFunc(t, unicode.IsLower) >= 0 {
			return "[secret]"
		}
		return t
	})
	for i, re := range r.exact {
		s = re.ReplaceAllLiteralString(s, r.label[i])
	}
	s = ipv4.ReplaceAllStringFunc(s, func(ip string) string {
		if privateIP(ip) {
			return ip
		}
		return "[ip]"
	})
	s = ipv6.ReplaceAllStringFunc(s, func(ip string) string {
		// Times (12:34:56) and file positions (main.go:12:3) look the same:
		// all digits, in at most three groups.
		decimal := strings.Trim(ip, "0123456789:") == "" && !strings.Contains(ip, "::")
		if decimal && strings.Count(ip, ":") <= 2 || privateIP(ip) {
			return ip
		}
		return "[ip]"
	})
	for _, n := range r.distinct {
		s = n.ReplaceAllString(s, "[sandbox]")
	}
	return s
}

// cleanMachine also removes every sandbox name, from text Cloudex made
// (commands and errors), where a name is never part of what someone meant.
func (r *redactor) cleanMachine(s string) string {
	s = r.clean(s)
	for _, n := range r.names {
		s = n.ReplaceAllString(s, "[sandbox]")
	}
	return s
}

type feedbackReport struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Body  string `json:"body"`
	At    int64  `json:"at"`
	// How it goes: with gh as this GitHub login, or, when empty, as a
	// new-issue link for the person to submit.
	Login string `json:"login,omitempty"`
}

func (rep feedbackReport) how() string {
	if rep.Login != "" {
		return "gh, as @" + rep.Login
	}
	return "GitHub's new-issue page, for you to submit"
}

// fence is a code fence longer than any run of backticks in s.
func fence(s string) string {
	f := "```"
	for strings.Contains(s, f) {
		f += "`"
	}
	return f
}

func firstLine(s string, n int) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	s = strings.TrimSpace(s)
	if len([]rune(s)) > n {
		s = string([]rune(s)[:n-1]) + "…"
	}
	return s
}

func buildReport(message string, last *lastError, setup []string, r *redactor) (feedbackReport, error) {
	message = strings.TrimSpace(r.clean(message))
	var b strings.Builder
	fmt.Fprintln(&b, feedbackMark)
	title := firstLine(message, 72)
	if message != "" {
		fmt.Fprintf(&b, "%s\n\n", message)
	}
	if last != nil {
		command, failure := r.cleanMachine(last.Command), r.cleanMachine(strings.TrimSpace(last.Error))
		if title == "" {
			title = firstLine(fmt.Sprintf("%s: %s", strings.TrimPrefix(command, "cloudex "), failure), 72)
		}
		when := time.UnixMilli(last.At).UTC().Format("2006-01-02 15:04 UTC")
		fmt.Fprintf(&b, "**What failed** (%s, cloudex %s)\n\n", when, r.cleanMachine(last.Version))
		f := fence(command + failure)
		fmt.Fprintf(&b, "%s\n$ %s\n%s\n%s\n\n", f, command, failure, f)
		if last.Crash != "" {
			stack := r.cleanMachine(last.Crash)
			if lines := strings.Split(stack, "\n"); len(lines) > 40 {
				stack = strings.Join(lines[:40], "\n") + "\n…"
			}
			f := fence(stack)
			fmt.Fprintf(&b, "<details><summary>Stack</summary>\n\n%s\n%s\n%s\n\n</details>\n\n", f, stack, f)
		}
	}
	if title == "" {
		return feedbackReport{}, errors.New(`say what happened, for example: cloudex feedback "fork hangs on a big sandbox", or add --last-error`)
	}
	fmt.Fprintln(&b, "**Setup**")
	for _, s := range setup {
		fmt.Fprintf(&b, "- %s\n", r.cleanMachine(s))
	}
	fmt.Fprintln(&b, "\n_Sent with `cloudex feedback`, which removed keys, addresses and the sandbox names it knew of._")
	rep := feedbackReport{Title: title, Body: b.String(), At: time.Now().UnixMilli()}
	sum := sha256.Sum256([]byte(rep.Title + "\x00" + rep.Body))
	rep.ID = hex.EncodeToString(sum[:])[:8]
	return rep, nil
}

// newIssueURL opens GitHub's new-issue page with the report filled in. A long
// body loses whole lines from its end, then characters, keeping its code
// fences and details closed, until the link fits.
func newIssueURL(repo string, rep feedbackReport) string {
	build := func(body string) string {
		return fmt.Sprintf("https://github.com/%s/issues/new?title=%s&body=%s", repo, url.QueryEscape(rep.Title), url.QueryEscape(body))
	}
	u := build(rep.Body)
	if len(u) <= maxIssueURL {
		return u
	}
	const note = "\n\n_(Cut to fit in a link; the rest is in the terminal that sent it.)_"
	closed := func(body string) string {
		// A fence is a run of 3+ backticks; one closes when a line holds only
		// backticks, at least as many as opened it.
		open := 0
		for _, l := range strings.Split(body, "\n") {
			t := strings.TrimSpace(l)
			n := len(t) - len(strings.TrimLeft(t, "`"))
			switch {
			case n < 3:
			case open == 0:
				open = n
			case n >= open && strings.Trim(t, "`") == "":
				open = 0
			}
		}
		if open > 0 {
			body += "\n" + strings.Repeat("`", open)
		}
		if strings.Count(body, "<details>") > strings.Count(body, "</details>") {
			body += "\n\n</details>"
		}
		return body + note
	}
	lines := strings.Split(rep.Body, "\n")
	for len(lines) > 2 {
		lines = lines[:len(lines)*4/5]
		if u = build(closed(strings.Join(lines, "\n"))); len(u) <= maxIssueURL {
			return u
		}
	}
	// One very long line: cut it by characters.
	rs := []rune(strings.Join(lines, "\n"))
	for len(rs) > 0 {
		rs = rs[:len(rs)*4/5]
		if u = build(closed(string(rs))); len(u) <= maxIssueURL {
			break
		}
	}
	return u
}

// ghLogin is the GitHub account gh would file as, or "" when gh can't.
func ghLogin() string {
	out, err := gh("", "api", "--hostname", "github.com", "user", "--jq", ".login")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// setupLines describes this computer and the deployment, without anything
// that identifies them.
func setupLines(version string, h *api.Health, reached bool) []string {
	lines := []string{fmt.Sprintf("CLI %s on %s/%s", version, runtime.GOOS, runtime.GOARCH)}
	switch {
	case h != nil:
		previews, backups := "off", "off"
		if h.PreviewZone != nil {
			previews = "on"
		}
		if h.Backups != nil && *h.Backups {
			backups = "on"
		}
		lines = append(lines, fmt.Sprintf("Deployment %s, preview links %s, backups %s", h.Version, previews, backups))
	case reached:
		lines = append(lines, "Deployment not reachable from this computer just now")
	default:
		lines = append(lines, "No deployment set up on this computer")
	}
	from := "a script or an agent"
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		from = "a terminal"
	}
	return append(lines, "Sent from "+from)
}

func newFeedbackCmd(version string, opts *options) *cobra.Command {
	var withLast, yes, web bool
	var sendID string
	cmd := &cobra.Command{
		Use:   "feedback [flags] [message]",
		Short: "Report a bug, an idea or a question to the Cloudex maintainers",
		Long: "Send feedback to the Cloudex maintainers as a public issue on\n" +
			"github.com/lucataco/cloudex. With --last-error, the report includes the last\n" +
			"command that failed (its flags' names, not their values) and its error. A message\n" +
			"of - is read from standard input. Flags go before the message.\n\n" +
			"Before anything is sent, Cloudex removes the deployment's key and addresses, link\n" +
			"tokens, the sandbox names it knows of, public IP addresses and anything that looks\n" +
			"like a secret, and shows you the report. In a terminal it then asks. It's filed\n" +
			"with the GitHub CLI (gh) when that's signed in, under that account; otherwise\n" +
			"GitHub's new-issue page opens with the report filled in, for you to submit.\n\n" +
			"Without a terminal, as when an agent runs it, or with a piped message, nothing\n" +
			"is sent: the report is printed and saved with an id. Show it to the person you\n" +
			"work for and, with their OK, send exactly that report, the way it said it would\n" +
			"go, with --send <id>.\n\n" +
			"Report security problems privately instead, at\n" +
			"https://github.com/lucataco/cloudex/security/advisories/new",
		Example: "  cloudex feedback \"fork is slow on a 15 GB sandbox\"\n" +
			"  cloudex feedback --last-error \"this happened while deploying\"\n" +
			"  cat notes.md | cloudex feedback -\n" +
			"  cloudex feedback --send 3f2a9c1d",
		RunE: func(cmd *cobra.Command, args []string) error {
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			repo := feedbackRepo()
			if sendID != "" {
				if len(args) > 0 || withLast {
					return errors.New("--send sends a saved report as it is: give no message or --last-error with it")
				}
				rep, err := loadDraft(sendID)
				if err != nil {
					return err
				}
				if web {
					rep.Login = ""
				}
				return sendReport(cmd, opts, repo, rep)
			}
			message := strings.Join(args, " ")
			if message == "-" {
				data, err := io.ReadAll(io.LimitReader(os.Stdin, 64<<10))
				if err != nil {
					return err
				}
				message = string(data)
			}
			var last *lastError
			if withLast {
				e, err := loadLastError()
				if err != nil {
					return err
				}
				last = e
			}
			rep, err := makeReport(cmd.Context(), version, opts, message, last)
			if err != nil {
				return err
			}
			if !web {
				rep.Login = ghLogin()
			}
			if yes {
				fmt.Fprintf(errOut, "Sending this report, with %s:\n\n%s\n\n%s\n", rep.how(), rep.Title, rep.Body)
				return sendReport(cmd, opts, repo, rep)
			}
			// A person answers on a terminal. An agent, a script or a piped
			// message gets a draft: the report, saved, and nothing sent.
			if message == "-" || !isTerminal(os.Stdin) || !isTerminal(os.Stderr) {
				writeConfigJSON("feedback-draft.json", rep)
				send := "cloudex feedback --send " + rep.ID
				if opts.json {
					return writeJSON(out, map[string]any{
						"sent": false, "id": rep.ID, "title": rep.Title, "body": rep.Body, "url": newIssueURL(repo, rep),
						"via": rep.how(), "send": send,
					})
				}
				fmt.Fprintf(out, "%s\n\n%s\n", rep.Title, rep.Body)
				fmt.Fprintf(errOut, "Not sent. Show this report to the person you work for. With their OK, send it as a public\nissue on github.com/%s, with %s, by running: %s\n", repo, rep.how(), send)
				return nil
			}
			fmt.Fprintf(errOut, "Title: %s\n\n%s\n", rep.Title, rep.Body)
			if !ask(cmd, fmt.Sprintf("This becomes a public issue on github.com/%s, filed with %s. Send it? [y/N] ", repo, rep.how())) {
				fmt.Fprintln(errOut, "Not sent.")
				return nil
			}
			return sendReport(cmd, opts, repo, rep)
		},
	}
	// A message is everything after the flags, so a "-y" in it is just text.
	cmd.Flags().SetInterspersed(false)
	cmd.Flags().BoolVar(&withLast, "last-error", false, "include the last command that failed and its error")
	cmd.Flags().StringVar(&sendID, "send", "", "send the report saved with this id, exactly as it was shown")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "send without asking; the report is printed as it goes")
	cmd.Flags().BoolVar(&web, "web", false, "open GitHub's new-issue page instead of filing with gh")
	return cmd
}

// makeReport builds a scrubbed report from everything this computer knows
// that a report mustn't show.
func makeReport(ctx context.Context, version string, opts *options, message string, last *lastError) (feedbackReport, error) {
	cfg, err := config.Load()
	if err != nil {
		cfg = &config.Config{}
	}
	values := []exactValue{
		{cfg.APIKey, "[admin key]"}, {os.Getenv("CLOUDEX_API_KEY"), "[admin key]"},
		{cfg.AccountID, "[account id]"}, {os.Getenv("CLOUDFLARE_ACCOUNT_ID"), "[account id]"},
		{cfg.PreviewZone, "[preview domain]"},
	}
	for _, u := range []string{cfg.APIURL, os.Getenv("CLOUDEX_API_URL"), opts.apiURL} {
		values = append(values, urlValues(u, "[API URL]")...)
	}
	if home, err := os.UserHomeDir(); err == nil && len(home) > 1 {
		values = append(values, exactValue{home, "~"})
	}
	values = append(values, identityValues()...)
	names := []string{cfg.Current}
	var health *api.Health
	reached := false
	if _, client, err := opts.load(); err == nil {
		reached = true
		values = append(values, urlValues(client.BaseURL, "[API URL]")...)
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if h, err := client.Health(hctx); err == nil {
			health = h
			if h.PreviewZone != nil {
				values = append(values, exactValue{*h.PreviewZone, "[preview domain]"})
			}
		}
		if list, err := client.List(hctx); err == nil {
			for _, s := range list {
				names = append(names, s.ID)
				if s.Name != nil {
					names = append(names, *s.Name)
				}
			}
		}
		cancel()
	}
	if last != nil {
		for _, v := range last.Private {
			values = append(values, exactValue{v, "[value]"})
			names = append(names, v)
		}
	}
	return buildReport(message, last, setupLines(version, health, reached), newRedactor(values, names))
}

func loadDraft(id string) (feedbackReport, error) {
	var rep feedbackReport
	p, err := configFile("feedback-draft.json")
	if err != nil {
		return rep, err
	}
	data, err := os.ReadFile(p)
	if err == nil {
		err = json.Unmarshal(data, &rep)
	}
	if err != nil || rep.ID != id {
		return rep, fmt.Errorf("no saved report %s: run cloudex feedback again to make one", id)
	}
	if time.Since(time.UnixMilli(rep.At)) > draftLifetime {
		return rep, fmt.Errorf("report %s is more than a day old: run cloudex feedback again", id)
	}
	return rep, nil
}

// sendReport sends a report the way it said it would go: with gh as its
// login, or as a new-issue link.
func sendReport(cmd *cobra.Command, opts *options, repo string, rep feedbackReport) error {
	out := cmd.OutOrStdout()
	link := newIssueURL(repo, rep)
	if rep.Login != "" {
		if now := ghLogin(); now != rep.Login {
			return fmt.Errorf("this report was to go with gh as @%s, but gh is %s now; run cloudex feedback again, or send it with --web",
				rep.Login, map[bool]string{true: "not signed in", false: "signed in as @" + now}[now == ""])
		}
		res, err := gh(rep.Body, "issue", "create", "--repo", "github.com/"+repo, "--title", rep.Title, "--body-file", "-")
		if err != nil {
			return fmt.Errorf("gh couldn't file it (%v); file it from this link instead:\n%s", err, link)
		}
		forgetDraft(rep.ID)
		issue := strings.TrimSpace(res)
		if lines := strings.Split(issue, "\n"); len(lines) > 1 {
			issue = lines[len(lines)-1]
		}
		if opts.json {
			return writeJSON(out, map[string]any{"sent": true, "url": issue})
		}
		_, err = fmt.Fprintf(out, "Filed %s\nThank you.\n", issue)
		return err
	}
	if opts.json {
		return writeJSON(out, map[string]any{"sent": false, "url": link})
	}
	if isTerminal(os.Stdout) {
		openBrowser(link)
	}
	_, err := fmt.Fprintf(out, "Submit the report on GitHub, where it's filled in:\n\n  %s\n", link)
	return err
}

func forgetDraft(id string) {
	if rep, err := loadDraft(id); err == nil && rep.ID == id {
		if p, err := configFile("feedback-draft.json"); err == nil {
			os.Remove(p)
		}
	}
}
