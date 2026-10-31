package cli

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGH stands in for the GitHub CLI: login is who gh is signed in as ("" for
// nobody), and each issue create is recorded with its body.
type fakeGH struct {
	login   string
	creates [][]string
	bodies  []string
}

func (f *fakeGH) install(t *testing.T) {
	old := gh
	t.Cleanup(func() { gh = old })
	gh = func(stdin string, args ...string) (string, error) {
		switch {
		case len(args) > 1 && args[0] == "api" && strings.Contains(strings.Join(args, " "), " user "):
			if f.login == "" {
				return "", errors.New("not logged in")
			}
			return f.login + "\n", nil
		case len(args) > 1 && args[0] == "issue" && args[1] == "create":
			f.creates = append(f.creates, args)
			f.bodies = append(f.bodies, stdin)
			return "Creating issue in lucataco/cloudex\n\nhttps://github.com/lucataco/cloudex/issues/7\n", nil
		}
		return "", errors.New("unexpected gh call")
	}
}

func feedbackAPI(t *testing.T, extra map[string]string) *fakeAPI {
	routes := map[string]string{
		"GET /v1/health":    `{"ok":true,"version":"v0.1.14","previewZone":"acme.dev","backups":true}`,
		"GET /v1/sandboxes": "[" + strings.Replace(sandboxJSON, `"name":"web"`, `"name":"payroll"`, 1) + "]",
	}
	for k, v := range extra {
		routes[k] = v
	}
	f := &fakeAPI{t: t, routes: routes}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Setenv("CLOUDEX_CONFIG_DIR", t.TempDir())
	t.Setenv("CLOUDEX_API_URL", srv.URL)
	t.Setenv("CLOUDEX_API_KEY", "test-key")
	t.Setenv("CLOUDEX_FEEDBACK_REPO", "")
	return f
}

func TestRedactorRemovesSecretsAndAddresses(t *testing.T) {
	values := append([]exactValue{
		{"cxk_0123456789abcdef", "[admin key]"}, {"5a4f569641634ff7b04add39e690ca24", "[account id]"}, {"acme.dev", "[preview domain]"},
	}, urlValues("https://cx.acme-corp.example", "[API URL]")...)
	r := newRedactor(append(values, exactValue{"cx-zzz999", "[value]"}), []string{"payroll", "acme-books"})
	text := "key cxk_0123456789abcdef at https://cx.acme-corp.example and lookup CX.ACME-CORP.EXAMPLE: no such host, " +
		"account 5a4f569641634ff7b04add39e690ca24, link https://3000-cx-abc123-a1b2c3d4e5.acme.dev/x, " +
		"encoded https%3A%2F%2F8080-cx-abc123-A1B2C3D4E5.ACME.DEV%2Fcb, other https://cloudex-api.someone.workers.dev, " +
		"sk-ant-api03-abcdefghijk, ghp_abcdefghijklmnopqrstuvwxyz0123456789, mail me@example.com, " +
		"Authorization: Bearer abcdefghijklmnop, Basic YWRtaW46UzNjcmV0UGFzcw==, GH_TOKEN=whatever123, " +
		`DB_PASSWORD="hunter2hunter2", {"client_secret": "s3cr3tvalue"}, --password hunter3hunter3, ` +
		"AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY, git https://me:pa55word@git.example.com/x, " +
		"from 203.0.113.9 and 2001:db8:85a3::8a2e:370:7334, at 12:34:56 on 10.0.0.1, " +
		"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\n-----END OPENSSH PRIVATE KEY-----, sandbox payroll, " +
		`{"accessToken": "abc123def456"}, clientSecret=s3cr3tvalue2, PGPASSWORD=hunter44 psql, STRIPE_KEY=rk_abcdefgh, ` +
		"mysql -pS3cretPass5, glpat-abcdefghijklmnopqrst, sk_live_abcdefghijklmnop12, " +
		"tok aGVsbG8gd29ybGQgdGhpcyBpcyBhIHNlY3JldA9+/Xy==, someone.workers.dev, " +
		"Preview https://3000-cx-zzz999-k3j4h5g6f7.acme.dev answers 502, at main.go:123:45"
	got := r.clean(text)
	for _, leaked := range []string{"cxk_0123", "acme-corp", "ACME-CORP", "5a4f5696", "a1b2c3d4e5", "A1B2C3D4E5", "acme.dev", "ACME.DEV",
		"someone", "abcdefghijk", "ghp_", "me@example.com", "abcdefghijklmnop", "YWRtaW46", "whatever123", "hunter2", "s3cr3tvalue",
		"hunter3", "wJalrXUtn", "pa55word", "203.0.113.9", "2001:db8", "b3BlbnNzaC1rZXk", "abc123def456", "s3cr3tvalue2",
		"hunter44", "rk_abcdefgh", "S3cretPass5", "glpat-", "sk_live", "aGVsbG8gd29y", "someone", "k3j4h5g6f7", "zzz999"} {
		if strings.Contains(got, leaked) {
			t.Errorf("%q survived: %s", leaked, got)
		}
	}
	for _, kept := range []string{"3000-cx-abc123-[token]", "12:34:56", "10.0.0.1", "payroll", "main.go:123:45"} {
		if !strings.Contains(got, kept) {
			t.Errorf("clean removed %q: %s", kept, got)
		}
	}
	if strings.Contains(r.cleanMachine(text), "payroll") {
		t.Error("cleanMachine kept a sandbox name")
	}
	if got := r.clean("the key provider is slow"); got != "the key provider is slow" {
		t.Errorf("prose changed: %s", got)
	}
	if got := r.clean("Acme-Books won't start"); got != "[sandbox] won't start" {
		t.Errorf("a name that's no ordinary word stayed: %s", got)
	}
	if got := newRedactor([]exactValue{{"luca", "[user]"}}, nil).clean("luca ran lucataco/cloudex"); got != "[user] ran lucataco/cloudex" {
		t.Errorf("a value was replaced inside a word: %s", got)
	}
}

func readLastError(t *testing.T) lastError {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("CLOUDEX_CONFIG_DIR"), "last-error.json"))
	if err != nil {
		t.Fatal(err)
	}
	var e lastError
	if err := json.Unmarshal(data, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestFailedCommandsKeepOnlyTheirShape(t *testing.T) {
	feedbackAPI(t, map[string]string{"POST /v1/sandboxes": `!503 {"error":"cx-aaa111 didn't start: no capacity"}`})
	run(t, "new", "--name", "acme-billing", "--size", "standard-4", "--repo", "https://github.com/acme/billing-core", "--env", "GH_TOKEN=ghp_x")
	e := readLastError(t)
	if e.Command != "cloudex new --env … --name … --repo … --size standard-4" {
		t.Fatalf("command %q", e.Command)
	}
	for _, v := range []string{"acme-billing", "https://github.com/acme/billing-core", "GH_TOKEN=ghp_x", "ghp_x"} {
		if !strings.Contains(strings.Join(e.Private, "\n"), v) {
			t.Errorf("private values miss %q: %v", v, e.Private)
		}
	}
	run(t, "exec", "payroll", "--", "curl", "-u", "admin:S3cret", "https://intra.example")
	if e := readLastError(t); strings.Contains(e.Command, "S3cret") || strings.Contains(e.Command, "intra") {
		t.Fatalf("command %q", e.Command)
	}
}

func TestFeedbackWithoutATerminalSavesADraftThatSendSends(t *testing.T) {
	feedbackAPI(t, nil)
	g := &fakeGH{login: "someone"}
	g.install(t)
	code, out, errOut := run(t, "--json=true", "feedback", "previews of payroll are slow")
	var preview struct {
		Sent                bool
		ID, Body, Send, Via string
	}
	if err := json.Unmarshal([]byte(out), &preview); err != nil || code != 0 || preview.Sent {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if !strings.Contains(preview.Body, "Deployment v0.1.14, preview links on, backups on") || preview.Via != "gh, as @someone" {
		t.Fatalf("preview %+v", preview)
	}
	if len(g.creates) != 0 {
		t.Fatal("sent without --send")
	}
	// Whatever changes meanwhile, --send sends exactly what was shown.
	if code, _, errOut := run(t, "feedback", "--send", preview.ID); code != 0 || len(g.bodies) != 1 || g.bodies[0] != preview.Body {
		t.Fatalf("code %d, stderr %q, sent %q", code, errOut, g.bodies)
	}
	if code, _, _ := run(t, "feedback", "--send", preview.ID); code == 0 {
		t.Fatal("a sent draft was sent again")
	}
	if code, _, _ := run(t, "feedback", "--send", "nope"); code == 0 {
		t.Fatal("an unknown id was accepted")
	}
}

func TestSendGoesTheWayTheDraftSaid(t *testing.T) {
	feedbackAPI(t, nil)
	g := &fakeGH{login: "someone"}
	g.install(t)
	draft := func(args ...string) string {
		_, out, _ := run(t, append([]string{"--json=true", "feedback"}, args...)...)
		var d struct{ ID, Via string }
		json.Unmarshal([]byte(out), &d)
		return d.ID + " " + d.Via
	}
	// Made for the web page: --send gives the link, though gh is signed in.
	id, via, _ := strings.Cut(draft("--web", "an idea"), " ")
	if via != "GitHub's new-issue page, for you to submit" {
		t.Fatalf("via %q", via)
	}
	if code, out, _ := run(t, "feedback", "--send", id); code != 0 || len(g.creates) != 0 || !strings.Contains(out, "/issues/new?") {
		t.Fatalf("code %d, stdout %q, gh %v", code, out, g.creates)
	}
	// Made for gh as @someone: refused once gh is someone else.
	id, _, _ = strings.Cut(draft("another idea"), " ")
	g.login = "other"
	if code, _, errOut := run(t, "feedback", "--send", id); code == 0 || !strings.Contains(errOut, "@other") || len(g.creates) != 0 {
		t.Fatalf("code %d, stderr %q", code, errOut)
	}
}

func TestUsageMistakesDontReplaceTheLastError(t *testing.T) {
	feedbackAPI(t, map[string]string{"POST /v1/sandboxes/payroll/stop": `!500 {"error":"disk full"}`})
	run(t, "stop", "payroll")
	run(t, "new", "-nacme-billing")
	run(t, "frobnicate", "acme")
	if e := readLastError(t); !strings.Contains(e.Error, "disk full") {
		t.Fatalf("last error %+v", e)
	}
}

func TestFeedbackFlagsComeBeforeTheMessage(t *testing.T) {
	feedbackAPI(t, nil)
	g := &fakeGH{login: "someone"}
	g.install(t)
	if code, out, _ := run(t, "feedback", "it fails when I pass", "-y"); code != 0 || len(g.creates) != 0 || !strings.Contains(out, "it fails when I pass -y") {
		t.Fatalf("a -y in the message sent it: code %d, %v", code, g.creates)
	}
	code, out, errOut := run(t, "feedback", "--yes", "fork is slow")
	if code != 0 || !strings.Contains(out, "https://github.com/lucataco/cloudex/issues/7") || !strings.Contains(errOut, "Sending this report") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out, errOut)
	}
	if strings.Join(g.creates[0], " ") != "issue create --repo github.com/lucataco/cloudex --title fork is slow --body-file -" {
		t.Fatalf("gh calls %v", g.creates)
	}
	if !strings.HasPrefix(g.bodies[0], feedbackMark) || strings.Contains(g.bodies[0], "test-key") || strings.Contains(g.bodies[0], "127.0.0.1:") {
		t.Fatalf("body %q", g.bodies[0])
	}
}

func TestFeedbackWithoutGHGivesTheLink(t *testing.T) {
	feedbackAPI(t, nil)
	(&fakeGH{}).install(t)
	code, out, _ := run(t, "feedback", "--yes", "an idea")
	if code != 0 || !strings.Contains(out, "https://github.com/lucataco/cloudex/issues/new?title=an+idea&body=") {
		t.Fatalf("code %d, stdout %q", code, out)
	}
}

func TestLastErrorIsReportedScrubbed(t *testing.T) {
	f := feedbackAPI(t, map[string]string{
		"POST /v1/sandboxes/payroll/stop": `!500 {"error":"couldn't save payroll, so it is still running: disk full"}`,
		"POST /v1/sandboxes/payroll/fork": `!409 {"error":"payroll is stopping"}`,
	})
	g := &fakeGH{login: "someone"}
	g.install(t)
	code, _, errOut := run(t, "stop", "payroll")
	if code != 1 || !strings.Contains(errOut, "cloudex feedback --last-error") {
		t.Fatalf("a server error should suggest a report: code %d, stderr %q", code, errOut)
	}
	// The sandbox list is down now: the name still goes, from what the command was given.
	f.routes["GET /v1/sandboxes"] = `!503 {"error":"down"}`
	if code, _, _ := run(t, "feedback", "--yes", "--last-error"); code != 0 || len(g.bodies) != 1 {
		t.Fatalf("code %d", code)
	}
	body := g.bodies[0]
	if !strings.Contains(body, "$ cloudex stop [arg]") || !strings.Contains(body, "disk full") || strings.Contains(body, "payroll") {
		t.Fatalf("body %q", body)
	}
	if !strings.HasPrefix(g.creates[0][5], "stop [arg]: ") {
		t.Fatalf("title %q", g.creates[0][5])
	}
	// A mistake of the user's isn't a bug, and a failed feedback doesn't replace the error.
	if _, _, errOut := run(t, "fork", "payroll"); strings.Contains(errOut, "feedback") {
		t.Fatalf("a 409 suggested a report: %q", errOut)
	}
	run(t, "feedback")
	run(t, "feedback", "--yes", "--last-error")
	if !strings.Contains(g.bodies[1], "is stopping") {
		t.Fatalf("last error after a failed feedback: %q", g.bodies[1])
	}
}

func TestNewIssueURLFitsAndClosesFences(t *testing.T) {
	long := newIssueURL("lucataco/cloudex", feedbackReport{Title: "t", Body: strings.Repeat("ünïcode ", 4000)})
	if len(long) > maxIssueURL {
		t.Fatalf("one long line: length %d", len(long))
	}
	body := feedbackMark + "\n" + strings.Repeat("a long report line ✓\n", 100) + "```text\n$ cloudex x\n" + strings.Repeat("error line\n", 2000) + "```\n"
	u := newIssueURL("lucataco/cloudex", feedbackReport{Title: "t", Body: body})
	q, err := url.Parse(u)
	if err != nil || len(u) > maxIssueURL {
		t.Fatalf("length %d, %v", len(u), err)
	}
	got := q.Query().Get("body")
	if strings.Count(got, "```")%2 != 0 || !strings.Contains(got, "Cut to fit") || !strings.Contains(got, "✓") {
		t.Fatalf("body %q", got)
	}
}
