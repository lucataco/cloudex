// An agent's events as lines for a person, as `cloudex agents logs` prints
// them (go/internal/cli/agentlog.go): prompts, the agent's messages, the
// commands it ran with the end of their output, files it changed, and how each
// turn ended. It reads Claude Code's stream-json, Codex's exec --json and
// OpenCode's run --format json. Everything here is shown as text, never HTML.
import type { AgentEvent, AgentKind } from "@cloudex/schema";

export type Line = { kind: "prompt" | "text" | "cmd" | "out" | "tool" | "end" | "error" | "gap"; text: string };

type Obj = Record<string, unknown>;
const str = (o: unknown, k: string) => (o && typeof o === "object" && typeof (o as Obj)[k] === "string" ? ((o as Obj)[k] as string) : "");
const obj = (o: unknown, k: string): Obj => {
  const v = o && typeof o === "object" ? (o as Obj)[k] : undefined;
  return v && typeof v === "object" && !Array.isArray(v) ? (v as Obj) : {};
};
const list = (o: unknown, k: string): Obj[] => {
  const v = o && typeof o === "object" ? (o as Obj)[k] : undefined;
  return Array.isArray(v) ? v.filter((x): x is Obj => !!x && typeof x === "object") : [];
};
const first = (...s: string[]) => s.find((x) => x) ?? "";
const oneLine = (s: string, n: number) => {
  const t = s.replace(/\s+/g, " ").trim();
  return t.length > n ? t.slice(0, n - 1) + "…" : t;
};
const cap = (s: string) => (s ? s[0]!.toUpperCase() + s.slice(1) : s);

// How many lines of a command's output show: the last ones, where test
// runners and builds put their summaries.
const OUTPUT_LINES = 4;
const EXIT_PREFIX = /^Exit code (\d+)(?:\n|$)/;
const SHELL_WRAPPER = /^(?:\/usr\/bin\/|\/bin\/)?(?:ba|z)?sh -lc '(.*)'$/s;

export class AgentLog {
  lines: Line[] = [];
  private started = new Map<number, number>();
  private cost = new Map<number, number>();
  private shells = new Set<string>();

  constructor(private agent: AgentKind) {}

  private add(kind: Line["kind"], text: string) {
    this.lines.push({ kind, text });
  }

  private output(text: string) {
    text = text.replace(/\n+$/, "");
    if (!text.trim()) return;
    let lines = text.split("\n");
    if (lines.length > OUTPUT_LINES) {
      this.add("out", `… ${lines.length - OUTPUT_LINES + 1} more lines`);
      lines = lines.slice(lines.length - OUTPUT_LINES + 1);
    }
    this.add("out", lines.join("\n"));
  }

  push(e: AgentEvent) {
    switch (e.type) {
      case "prompt":
        this.started.set(e.turn, e.at);
        if (e.turn > 1) this.add("gap", "");
        this.add("prompt", e.text ?? "");
        break;
      case "output":
        this.add("text", (e.text ?? "") + (e.truncated ? " [cut]" : ""));
        break;
      case "event":
        if (!e.event) break;
        if (this.agent === "codex") this.codex(e.event);
        else if (this.agent === "opencode") this.opencode(e.turn, e.event);
        else this.claude(e.turn, e.event);
        break;
      case "end":
        this.end(e);
        break;
    }
  }

  private end(e: AgentEvent) {
    const at = this.started.get(e.turn);
    let took = at !== undefined && e.at >= at ? ` in ${Math.round((e.at - at) / 1000)}s` : "";
    const c = this.cost.get(e.turn) ?? 0;
    if (c >= 0.005) took += `, $${c.toFixed(2)}`;
    if (e.interrupted) this.add("end", `turn ${e.turn} interrupted${took}`);
    else if (e.error) this.add("error", `turn ${e.turn} failed${took}: ${oneLine(e.error, 300)}`);
    else if (e.exitCode !== undefined && e.exitCode !== 0) this.add("error", `turn ${e.turn} failed${took} (exit ${e.exitCode})`);
    else this.add("end", `turn ${e.turn} done${took}`);
  }

  private claude(turn: number, ev: Obj) {
    const blocks = list(obj(ev, "message"), "content");
    switch (str(ev, "type")) {
      case "assistant":
        for (const b of blocks) {
          if (str(b, "type") === "text") {
            const t = str(b, "text").trim();
            if (t) this.add("text", t);
          } else if (str(b, "type") === "tool_use") {
            if (str(b, "name") === "Bash") this.shells.add(str(b, "id"));
            this.tool(str(b, "name"), obj(b, "input"));
          }
        }
        break;
      case "user":
        for (const b of blocks) {
          if (str(b, "type") !== "tool_result") continue;
          const text = resultText(b["content"]);
          const shell = this.shells.has(str(b, "tool_use_id"));
          const m = EXIT_PREFIX.exec(text);
          if (shell && m) {
            this.output(text.slice(m[0].length));
            this.add("error", `exit ${m[1]}`);
          } else if (b["is_error"] === true) this.add("error", `error: ${oneLine(text, 300)}`);
          else if (shell) this.output(text);
        }
        break;
      case "result":
        if (typeof ev["total_cost_usd"] === "number") this.cost.set(turn, ev["total_cost_usd"] as number);
        if (ev["is_error"] === true) this.add("error", `error: ${oneLine(first(str(ev, "result"), str(ev, "subtype")), 300)}`);
        break;
    }
  }

  private tool(name: string, input: Obj) {
    if (name === "Bash") return this.add("cmd", str(input, "command"));
    if (name === "TodoWrite") return;
    const arg = first(
      str(input, "file_path"),
      str(input, "notebook_path"),
      str(input, "path"),
      str(input, "pattern"),
      str(input, "url"),
      str(input, "query"),
      str(input, "description"),
      str(input, "prompt"),
    );
    this.add("tool", `${name} ${oneLine(arg, 160)}`.trim());
  }

  private codex(ev: Obj) {
    const item = obj(ev, "item");
    const kind = str(item, "type");
    const typ = str(ev, "type");
    if (typ === "item.started" && kind === "command_execution") {
      let cmd = str(item, "command");
      const m = SHELL_WRAPPER.exec(cmd);
      if (m) cmd = m[1]!.replaceAll(`'\\''`, "'");
      this.add("cmd", cmd);
      return;
    }
    if (typ !== "item.completed") {
      if (typ === "turn.failed") this.add("error", `error: ${oneLine(str(obj(ev, "error"), "message"), 300)}`);
      else if (typ === "error") this.add("error", `error: ${oneLine(str(ev, "message"), 300)}`);
      return;
    }
    switch (kind) {
      case "agent_message":
        this.add("text", str(item, "text").trim());
        break;
      case "command_execution": {
        this.output(str(item, "aggregated_output"));
        const code = item["exit_code"];
        if (typeof code === "number" && code !== 0) this.add("error", `exit ${code}`);
        break;
      }
      case "file_change": {
        const verbs: Record<string, string> = { add: "Add", delete: "Delete", update: "Edit" };
        for (const c of list(item, "changes")) this.add("tool", `${verbs[str(c, "kind")] ?? "Change"} ${str(c, "path")}`);
        break;
      }
      case "web_search":
        this.add("tool", `Search ${oneLine(str(item, "query"), 160)}`);
        break;
      case "mcp_tool_call":
        this.add("tool", `Tool ${str(item, "server")}/${str(item, "tool")}`);
        break;
      case "error":
        this.add("error", `error: ${oneLine(str(item, "message"), 300)}`);
        break;
    }
  }

  private opencode(turn: number, ev: Obj) {
    const part = obj(ev, "part");
    switch (str(ev, "type")) {
      case "text": {
        const t = str(part, "text").trim();
        if (t) this.add("text", t);
        break;
      }
      case "tool_use": {
        const state = obj(part, "state");
        const input = obj(state, "input");
        const tool = str(part, "tool");
        if (tool === "shell" || tool === "bash") {
          this.add("cmd", str(input, "command"));
          this.output(str(state, "output"));
          const code = obj(obj(state, "metadata"), "metadata")["exit"];
          if (typeof code === "number" && code !== 0) this.add("error", `exit ${code}`);
        } else if (tool !== "todowrite" && tool !== "todoread") {
          const arg = first(
            str(input, "path"),
            str(input, "filePath"),
            str(input, "file_path"),
            str(input, "pattern"),
            str(input, "url"),
            str(input, "query"),
            str(input, "description"),
            str(input, "prompt"),
          );
          this.add("tool", `${cap(tool)} ${oneLine(arg, 160)}`.trim());
        }
        if (str(state, "status") === "error") this.add("error", `error: ${oneLine(first(str(state, "error"), str(state, "output")), 300)}`);
        break;
      }
      case "step_finish":
        if (typeof part["cost"] === "number") this.cost.set(turn, (this.cost.get(turn) ?? 0) + (part["cost"] as number));
        break;
      case "error": {
        const err = obj(ev, "error");
        this.add("error", `error: ${oneLine(first(str(err, "message"), str(err, "type")), 300)}`);
        break;
      }
    }
  }
}

function resultText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .filter((o): o is Obj => !!o && typeof o === "object" && (o as Obj)["type"] === "text")
    .map((o) => str(o, "text"))
    .join("\n");
}
