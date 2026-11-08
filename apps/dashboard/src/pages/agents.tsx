// The agents tab: Claude Code, Codex and OpenCode conversations in the
// sandbox. The list, a new agent, and one conversation, followed with long
// polls on its events and shown as text.
import type { Agent, AgentEvents, AgentKind, Me, Sandbox } from "@cloudex/schema";
import { useEffect, useMemo, useRef, useState } from "preact/hooks";
import { ApiError, get, post, sb } from "../api";
import { AgentLog, type Line } from "../agentlog";
import { Link, match, navigate, useLocation } from "../router";
import { Act, Loading, Problem, State, ago, usePoll } from "../ui";
import { canWrite } from "./sandboxes";

const NAMES: Record<AgentKind, string> = { claude: "Claude Code", codex: "Codex", opencode: "OpenCode" };
const MODEL_HINT: Record<AgentKind, string> = {
  claude: "opus, sonnet, haiku, or a full model id",
  codex: "gpt-5, gpt-5-codex…",
  opencode: "provider/model",
};

export function Agents(props: { box: Sandbox; me: Me }) {
  const { path } = useLocation();
  const one = match("/s/:ref/agents/:id", path);
  if (one) return <Conversation box={props.box} id={one.id!} me={props.me} />;
  return <AgentList box={props.box} me={props.me} />;
}

function AgentList(props: { box: Sandbox; me: Me }) {
  const ref = props.box.name ?? props.box.id;
  const list = usePoll(() => get<Agent[]>(`${sb(props.box.id)}/agents`), [props.box.id], 5000);
  const [making, setMaking] = useState(false);
  const write = canWrite(props.me);
  return (
    <>
      <div class="head sub">
        <h2>Agents</h2>
        {write ? (
          <button type="button" class="btn btn-primary" onClick={() => setMaking(!making)}>
            {making ? "Close" : "New agent"}
          </button>
        ) : null}
      </div>
      {making ? <NewAgent box={props.box} /> : null}
      <Problem error={list.error} />
      {!list.data ? (
        <Loading />
      ) : list.data.length === 0 ? (
        <p class="muted">No agents in {ref} yet.</p>
      ) : (
        <table class="list">
          <thead>
            <tr>
              <th>Agent</th>
              <th>State</th>
              <th>Turns</th>
              <th>Last message</th>
              <th>Updated</th>
            </tr>
          </thead>
          <tbody>
            {list.data.map((a) => (
              <tr key={a.id}>
                <td>
                  <Link href={`/s/${encodeURIComponent(ref)}/agents/${a.id}`} class="strong">
                    {NAMES[a.agent]}
                  </Link>
                  <span class="id">{a.id}</span>
                  {a.model ? <div class="small muted mono">{a.model}</div> : null}
                </td>
                <td>
                  <State state={a.state} />
                  {a.queued ? <span class="small muted"> +{a.queued} queued</span> : null}
                </td>
                <td>{a.turns}</td>
                <td class="clip-text">{a.error ? <span class="problem-text">{a.error}</span> : (a.lastMessage ?? "–")}</td>
                <td>{ago(a.updatedAt)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function NewAgent(props: { box: Sandbox }) {
  const [agent, setAgent] = useState<AgentKind>("claude");
  const [model, setModel] = useState("");
  const [cwd, setCwd] = useState("");
  const [prompt, setPrompt] = useState("");
  const [error, setError] = useState<string | null>(null);
  const ref = props.box.name ?? props.box.id;
  const start = async () => {
    const a = await post<Agent>(`${sb(props.box.id)}/agents`, {
      agent,
      prompt,
      ...(model.trim() ? { model: model.trim() } : {}),
      ...(cwd.trim() ? { cwd: cwd.trim() } : {}),
    });
    navigate(`/s/${encodeURIComponent(ref)}/agents/${a.id}`);
  };
  return (
    <section class="card">
      <div class="form grid">
        <label>
          Agent
          <select value={agent} onChange={(e) => setAgent(e.currentTarget.value as AgentKind)}>
            {(Object.keys(NAMES) as AgentKind[]).map((k) => (
              <option value={k} key={k}>
                {NAMES[k]}
              </option>
            ))}
          </select>
        </label>
        <label>
          Model (optional)
          <input value={model} placeholder={MODEL_HINT[agent]} onInput={(e) => setModel(e.currentTarget.value)} />
        </label>
        <label>
          Directory (optional)
          <input value={cwd} placeholder="~ (the home directory)" onInput={(e) => setCwd(e.currentTarget.value)} />
        </label>
      </div>
      <label class="block">
        Task
        <textarea rows={5} value={prompt} onInput={(e) => setPrompt(e.currentTarget.value)} placeholder="What should it do?" />
      </label>
      <Act run={start} onError={setError} kind="primary" disabled={!prompt.trim()} busy="Starting…">
        Start
      </Act>
      <Problem error={error} />
    </section>
  );
}

function Conversation(props: { box: Sandbox; id: string; me: Me }) {
  const [agent, setAgent] = useState<Agent | null>(null);
  const [lines, setLines] = useState<Line[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [prompt, setPrompt] = useState("");
  const bottom = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const log = useMemo(() => ({ current: null as AgentLog | null }), [props.id]);
  const write = canWrite(props.me);
  const ref = props.box.name ?? props.box.id;

  useEffect(() => {
    let gone = false;
    let after = 0;
    (async () => {
      while (!gone) {
        try {
          const r = await get<AgentEvents>(`${sb(props.box.id)}/agents/${props.id}/events?after=${after}&wait=${after ? 25 : 0}`);
          if (gone) return;
          log.current ??= new AgentLog(r.agent.agent);
          for (const e of r.events) log.current.push(e);
          after = r.next;
          setAgent(r.agent);
          setLines([...log.current.lines]);
          setError(null);
        } catch (e) {
          if (gone) return;
          setError(e instanceof Error ? e.message : String(e));
          if (e instanceof ApiError && e.status === 404) return;
          await new Promise((r) => setTimeout(r, 3000));
        }
      }
    })();
    return () => {
      gone = true;
    };
  }, [props.id]);

  useEffect(() => {
    const onScroll = () => {
      stick.current = innerHeight + scrollY >= document.body.scrollHeight - 80;
    };
    addEventListener("scroll", onScroll, { passive: true });
    return () => removeEventListener("scroll", onScroll);
  }, []);
  useEffect(() => {
    if (stick.current) bottom.current?.scrollIntoView({ block: "end" });
  }, [lines.length]);

  const send = async (interrupt: boolean) => {
    await post(`${sb(props.box.id)}/agents/${props.id}/messages`, { prompt, interrupt });
    setPrompt("");
  };

  return (
    <>
      <div class="head sub">
        <Link href={`/s/${encodeURIComponent(ref)}/agents`}>← Agents</Link>
        <h2>
          {agent ? NAMES[agent.agent] : "Agent"} <span class="id">{props.id}</span>
        </h2>
        {agent ? <State state={agent.state} /> : null}
        {agent?.model ? <span class="mono small muted">{agent.model}</span> : null}
        {agent ? <span class="mono small muted">{agent.cwd}</span> : null}
      </div>
      <Problem error={error ?? agent?.error} />
      {!agent && !error ? <Loading what="the conversation" /> : null}
      <div class="log" role="log" aria-live="polite">
        {lines.map((l, i) => (
          <div class={`l l-${l.kind}`} key={i}>
            {l.kind === "prompt" ? "> " : l.kind === "cmd" ? "$ " : l.kind === "end" ? "— " : ""}
            {l.text}
          </div>
        ))}
        {agent?.state === "running" ? <div class="l l-end working">working…</div> : null}
        <div ref={bottom} />
      </div>
      {write && agent ? (
        <section class="send">
          <textarea
            rows={3}
            value={prompt}
            placeholder={agent.state === "running" ? "A message waits for the running turn, unless you interrupt it" : "Your next message"}
            onInput={(e) => setPrompt(e.currentTarget.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter" && (e.metaKey || e.ctrlKey) && prompt.trim()) {
                e.preventDefault();
                send(false).catch((x) => setError(x.message));
              }
            }}
          />
          <div class="row">
            <Act run={() => send(false)} onError={setError} kind="primary" disabled={!prompt.trim()} busy="Sending…">
              Send
            </Act>
            {agent.state === "running" ? (
              <>
                <Act run={() => send(true)} onError={setError} disabled={!prompt.trim()} title="Stop the running turn and drop queued messages, then send this">
                  Interrupt and send
                </Act>
                <Act run={() => post(`${sb(props.box.id)}/agents/${props.id}/interrupt`)} onError={setError} kind="danger">
                  Interrupt
                </Act>
              </>
            ) : null}
            <span class="muted small">⌘/Ctrl + Enter sends</span>
          </div>
        </section>
      ) : null}
    </>
  );
}
