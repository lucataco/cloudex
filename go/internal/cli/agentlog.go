package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/lucataco/cloudex/go/internal/api"
)

// agentPrinter turns an agent's events into lines for a person: the prompts,
// the agent's messages, the commands it ran, the files it changed, and how
// each turn ended. It reads Claude Code's stream-json and Codex's exec --json.
type agentPrinter struct {
	w       io.Writer
	agent   string
	started map[int]int64   // turn -> when its prompt was sent
	cost    map[int]float64 // turn -> Claude Code's reported cost
	shells  map[string]bool // ids of Claude Code's Bash calls, whose output is shown
	err     error
}

func newAgentPrinter(w io.Writer, agent string) *agentPrinter {
	return &agentPrinter{w: w, agent: agent, started: map[int]int64{}, cost: map[int]float64{}, shells: map[string]bool{}}
}

// How many lines of a command's output are shown: the last ones, where
// test runners and builds put their summaries.
const outputLines = 4

// output prints the end of a command's output, indented.
func (p *agentPrinter) output(text string) {
	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return
	}
	lines := strings.Split(text, "\n")
	if len(lines) > outputLines {
		p.line("  … %d more lines", len(lines)-outputLines+1)
		lines = lines[len(lines)-outputLines+1:]
	}
	for _, l := range lines {
		p.line("  %s", l)
	}
}

func (p *agentPrinter) line(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format+"\n", args...)
	}
}

// block prints text, with prefix on its first line and indent on the rest.
func (p *agentPrinter) block(prefix, indent, text string) {
	for i, l := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		if i == 0 {
			p.line("%s%s", prefix, l)
		} else {
			p.line("%s%s", indent, l)
		}
	}
}

func (p *agentPrinter) print(e api.AgentEvent) error {
	switch e.Type {
	case "prompt":
		p.started[e.Turn] = e.At
		if e.Turn > 1 {
			p.line("")
		}
		p.block("> ", "  ", e.Text)
	case "output":
		text := e.Text
		if e.Truncated {
			text += " [cut]"
		}
		p.block("", "", text)
	case "event":
		var ev map[string]any
		if json.Unmarshal(e.Event, &ev) == nil {
			if p.agent == "codex" {
				p.codex(ev)
			} else {
				p.claude(e.Turn, ev)
			}
		}
	case "end":
		p.end(e)
	}
	return p.err
}

func (p *agentPrinter) end(e api.AgentEvent) {
	took := ""
	if at, ok := p.started[e.Turn]; ok && e.At >= at {
		took = " in " + (time.Duration(e.At-at) * time.Millisecond).Round(time.Second).String()
	}
	if c := p.cost[e.Turn]; c >= 0.005 {
		took += fmt.Sprintf(", $%.2f", c)
	}
	switch {
	case e.Interrupted:
		p.line("-- turn %d interrupted%s", e.Turn, took)
	case e.Error != nil && *e.Error != "":
		p.line("-- turn %d failed%s: %s", e.Turn, took, oneLine(*e.Error, 200))
	case e.ExitCode != nil && *e.ExitCode != 0:
		p.line("-- turn %d failed%s (exit %d)", e.Turn, took, *e.ExitCode)
	default:
		p.line("-- turn %d done%s", e.Turn, took)
	}
}

// claude reads Claude Code's stream-json: assistant messages with text and
// tool calls, user messages with tool results, and a result at the end.
func (p *agentPrinter) claude(turn int, ev map[string]any) {
	switch str(ev, "type") {
	case "assistant":
		for _, b := range blocks(ev) {
			switch str(b, "type") {
			case "text":
				if t := strings.TrimSpace(str(b, "text")); t != "" {
					p.block("", "", t)
				}
			case "tool_use":
				if str(b, "name") == "Bash" {
					p.shells[str(b, "id")] = true
				}
				p.tool(str(b, "name"), obj(b, "input"))
			}
		}
	case "user":
		for _, b := range blocks(ev) {
			if str(b, "type") != "tool_result" {
				continue
			}
			text := resultText(b["content"])
			shell := p.shells[str(b, "tool_use_id")]
			// A command that failed: "Exit code 3", then its output.
			if m := exitCodePrefix.FindStringSubmatch(text); shell && m != nil {
				p.output(text[len(m[0]):])
				p.line("  exit %s", m[1])
			} else if b["is_error"] == true {
				p.line("  error: %s", oneLine(text, 200))
			} else if shell {
				p.output(text)
			}
		}
	case "result":
		if c, ok := ev["total_cost_usd"].(float64); ok {
			p.cost[turn] = c
		}
		if ev["is_error"] == true {
			p.line("error: %s", oneLine(firstNonEmpty(str(ev, "result"), str(ev, "subtype")), 200))
		}
	}
}

// tool prints one of Claude Code's tool calls: the command for Bash, the file
// or pattern for the others.
func (p *agentPrinter) tool(name string, in map[string]any) {
	switch name {
	case "Bash":
		p.block("$ ", "  ", str(in, "command"))
		return
	case "TodoWrite":
		return
	}
	arg := firstNonEmpty(str(in, "file_path"), str(in, "notebook_path"), str(in, "path"), str(in, "pattern"),
		str(in, "url"), str(in, "query"), str(in, "description"), str(in, "prompt"))
	p.line("%s", strings.TrimSpace(name+" "+oneLine(arg, 120)))
}

var exitCodePrefix = regexp.MustCompile(`^Exit code (\d+)(?:\n|$)`)

var shellWrapper = regexp.MustCompile(`^(?:/usr/bin/|/bin/)?(?:ba|z)?sh -lc '(.*)'$`)

// codex reads Codex's exec --json: items started and completed, such as
// commands, messages and file changes, and turn failures.
func (p *agentPrinter) codex(ev map[string]any) {
	item := obj(ev, "item")
	kind, typ := str(item, "type"), str(ev, "type")
	switch {
	case typ == "item.started" && kind == "command_execution":
		cmd := str(item, "command")
		if m := shellWrapper.FindStringSubmatch(cmd); m != nil {
			cmd = strings.ReplaceAll(m[1], `'\''`, "'")
		}
		p.block("$ ", "  ", cmd)
	case typ != "item.completed":
		switch typ {
		case "turn.failed":
			p.line("error: %s", oneLine(str(obj(ev, "error"), "message"), 200))
		case "error":
			p.line("error: %s", oneLine(str(ev, "message"), 200))
		}
	case kind == "agent_message":
		p.block("", "", strings.TrimSpace(str(item, "text")))
	case kind == "command_execution":
		p.output(str(item, "aggregated_output"))
		if code, ok := item["exit_code"].(float64); ok && code != 0 {
			p.line("  exit %d", int(code))
		}
	case kind == "file_change":
		verbs := map[string]string{"add": "Add", "delete": "Delete", "update": "Edit"}
		for _, c := range list(item, "changes") {
			p.line("%s %s", firstNonEmpty(verbs[str(c, "kind")], "Change"), str(c, "path"))
		}
	case kind == "web_search":
		p.line("Search %s", oneLine(str(item, "query"), 120))
	case kind == "mcp_tool_call":
		p.line("Tool %s/%s", str(item, "server"), str(item, "tool"))
	case kind == "error":
		p.line("error: %s", oneLine(str(item, "message"), 200))
	}
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func obj(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}

func list(m map[string]any, key string) []map[string]any {
	var out []map[string]any
	items, _ := m[key].([]any)
	for _, it := range items {
		if o, ok := it.(map[string]any); ok {
			out = append(out, o)
		}
	}
	return out
}

// blocks are the content blocks of a Claude Code message event.
func blocks(ev map[string]any) []map[string]any {
	return list(obj(ev, "message"), "content")
}

// resultText is a tool result's text: a string, or text blocks.
func resultText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var parts []string
	items, _ := content.([]any)
	for _, it := range items {
		if o, ok := it.(map[string]any); ok && str(o, "type") == "text" {
			parts = append(parts, str(o, "text"))
		}
	}
	return strings.Join(parts, "\n")
}

// writeJSONLine writes v as one line of JSON, for streams of events.
func writeJSONLine(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
