// The commands tab: background commands in the sandbox. The list, a new
// one, and one command's output, followed with long polls.
import type { Command, CommandOutput, Me, Sandbox } from "@cloudex/schema";
import { useEffect, useRef, useState } from "preact/hooks";
import { ApiError, del, get, post, sb } from "../api";
import { Link, match, navigate, useLocation } from "../router";
import { Act, Loading, Problem, State, ago, bytes, usePoll } from "../ui";
import { canWrite } from "./sandboxes";

export function Commands(props: { box: Sandbox; me: Me }) {
  const { path } = useLocation();
  const one = match("/s/:ref/commands/:id", path);
  if (one) return <Output box={props.box} id={one.id!} me={props.me} />;
  return <CommandList box={props.box} me={props.me} />;
}

const ended = (c: Command) =>
  c.state === "exited" ? `exit ${c.exitCode}` : c.state === "killed" ? `${c.timedOut ? "timed out" : "killed"} (${c.signal})` : c.state === "lost" ? "lost in a stop" : "";

function CommandList(props: { box: Sandbox; me: Me }) {
  const ref = props.box.name ?? props.box.id;
  const list = usePoll(() => get<Command[]>(`${sb(props.box.id)}/commands`), [props.box.id], 5000);
  const write = canWrite(props.me);
  const [cmd, setCmd] = useState("");
  const [root, setRoot] = useState(false);
  const [cwd, setCwd] = useState("");
  const [timeout, setTimeoutSec] = useState("0");
  const [error, setError] = useState<string | null>(null);
  const start = async () => {
    const c = await post<Command>(`${sb(props.box.id)}/commands`, {
      cmd,
      root,
      ...(cwd.trim() ? { cwd: cwd.trim() } : {}),
      timeoutSec: Number(timeout) || 0,
    });
    navigate(`/s/${encodeURIComponent(ref)}/commands/${c.id}`);
  };
  return (
    <>
      <h2>Commands</h2>
      {write ? (
        <section class="card">
          <label class="block">
            Run in the background
            <textarea rows={2} class="mono" value={cmd} placeholder="npm run dev" onInput={(e) => setCmd(e.currentTarget.value)} />
          </label>
          <div class="form grid">
            <label>
              Directory (optional)
              <input value={cwd} placeholder="~" onInput={(e) => setCwd(e.currentTarget.value)} />
            </label>
            <label>
              Stop after (seconds; 0 for never)
              <input type="number" min="0" value={timeout} onInput={(e) => setTimeoutSec(e.currentTarget.value)} />
            </label>
            <label class="check">
              <input type="checkbox" checked={root} onChange={(e) => setRoot(e.currentTarget.checked)} />
              As root
            </label>
          </div>
          <Act run={start} onError={setError} kind="primary" disabled={!cmd.trim()}>
            Start
          </Act>
          <Problem error={error} />
        </section>
      ) : null}
      <Problem error={list.error} />
      {!list.data ? (
        <Loading />
      ) : list.data.length === 0 ? (
        <p class="muted">No background commands in {ref}.</p>
      ) : (
        <table class="list">
          <thead>
            <tr>
              <th>Command</th>
              <th>State</th>
              <th>Output</th>
              <th>Started</th>
            </tr>
          </thead>
          <tbody>
            {list.data.map((c) => (
              <tr key={c.id}>
                <td>
                  <Link href={`/s/${encodeURIComponent(ref)}/commands/${c.id}`} class="mono strong clip-text">
                    {c.cmd}
                  </Link>
                  <span class="id">{c.id}</span>
                  {c.root ? <span class="small muted"> as root</span> : null}
                </td>
                <td>
                  <State state={c.state} />
                  <span class="small muted"> {ended(c)}</span>
                </td>
                <td>{bytes(c.outputBytes)}</td>
                <td>{ago(c.startedAt)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

// The log holds at most this much text in the page; older output drops off the top.
const KEEP = 2_000_000;

function Output(props: { box: Sandbox; id: string; me: Me }) {
  const ref = props.box.name ?? props.box.id;
  const base = `${sb(props.box.id)}/commands/${props.id}`;
  const [command, setCommand] = useState<Command | null>(null);
  const [text, setText] = useState("");
  const [dropped, setDropped] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [signal, setSignal] = useState("TERM");
  const pre = useRef<HTMLPreElement>(null);
  const stick = useRef(true);
  const write = canWrite(props.me);

  useEffect(() => {
    let gone = false;
    let after = 0;
    let all = "";
    (async () => {
      while (!gone) {
        try {
          const r = await get<CommandOutput>(`${base}/output?after=${after}&wait=${after ? 25 : 0}`);
          if (gone) return;
          all += r.data;
          if (all.length > KEEP) {
            setDropped((d) => d + all.length - KEEP);
            all = all.slice(all.length - KEEP);
          }
          after = r.next;
          setText(all);
          setCommand(r.command);
          setError(null);
          if (r.command.state !== "running" && !r.data) return;
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
    const p = pre.current;
    if (p && stick.current) p.scrollTop = p.scrollHeight;
  }, [text]);

  return (
    <>
      <div class="head sub">
        <Link href={`/s/${encodeURIComponent(ref)}/commands`}>← Commands</Link>
        <h2 class="mono clip-text">{command?.cmd ?? props.id}</h2>
        {command ? <State state={command.state} /> : null}
        {command ? <span class="small muted">{ended(command)}</span> : null}
      </div>
      {command ? (
        <p class="muted small">
          {props.id} · started {ago(command.startedAt)} in <span class="mono">{command.cwd}</span>
          {command.root ? " as root" : ""}
          {command.endedAt ? ` · ended ${ago(command.endedAt)}` : ""}
        </p>
      ) : null}
      <Problem error={error} />
      {dropped ? <p class="muted small">The first {bytes(dropped)} of output aren't shown here.</p> : null}
      <pre
        class="output"
        ref={pre}
        onScroll={(e) => {
          const p = e.currentTarget;
          stick.current = p.scrollTop + p.clientHeight >= p.scrollHeight - 40;
        }}
      >
        {text || (command ? "(no output yet)" : "")}
      </pre>
      {write && command ? (
        <div class="row">
          {command.state === "running" ? (
            <>
              <select value={signal} onChange={(e) => setSignal(e.currentTarget.value)} aria-label="Signal">
                {["TERM", "INT", "HUP", "KILL"].map((s) => (
                  <option key={s}>{s}</option>
                ))}
              </select>
              <Act run={() => post(`${base}/kill`, { signal })} onError={setError} kind="danger">
                Send the signal
              </Act>
            </>
          ) : (
            <Act
              run={async () => {
                await del(base);
                navigate(`/s/${encodeURIComponent(ref)}/commands`);
              }}
              onError={setError}
              kind="danger"
              confirm="Remove this command and its output?"
            >
              Remove
            </Act>
          )}
        </div>
      ) : null}
    </>
  );
}
