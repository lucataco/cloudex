// /: the sandboxes, refreshed every 5 seconds, and a new one.
import type { Me, Sandbox, Save } from "@cloudex/schema";
import { useState } from "preact/hooks";
import { del, get, post, sb } from "../api";
import { Link, navigate } from "../router";
import { Act, Loading, Problem, SIZES, SIZE_NOTE, State, ago, bytes, usePoll } from "../ui";

export const canWrite = (me: Me) => me.role !== "read";

export function Sandboxes(props: { me: Me }) {
  const list = usePoll(() => get<Sandbox[]>("/v1/sandboxes"), [], 5000);
  const [error, setError] = useState<string | null>(null);
  const [making, setMaking] = useState(false);
  const write = canWrite(props.me);
  return (
    <>
      <div class="head">
        <h1>Sandboxes</h1>
        {write ? (
          <button type="button" class="btn btn-primary" onClick={() => setMaking(!making)}>
            {making ? "Close" : "New sandbox"}
          </button>
        ) : null}
      </div>
      {making ? <NewSandbox onMade={(s) => navigate(`/s/${s.name ?? s.id}`)} /> : null}
      <Problem error={error ?? list.error} />
      {!list.data ? (
        <Loading />
      ) : list.data.length === 0 ? (
        <p class="muted">No sandboxes yet.{write ? " Make one with New sandbox, or cloudex new." : ""}</p>
      ) : (
        <table class="list">
          <thead>
            <tr>
              <th>Name</th>
              <th>State</th>
              <th>Size</th>
              <th>Idle stop</th>
              <th>Started</th>
              <th>Made</th>
              {write ? <th /> : null}
            </tr>
          </thead>
          <tbody>
            {list.data.map((s) => (
              <tr key={s.id}>
                <td>
                  <Link href={`/s/${s.name ?? s.id}`} class="strong">
                    {s.name ?? s.id}
                  </Link>
                  {s.name ? <span class="id">{s.id}</span> : null}
                  {s.error ? <div class="small problem-text">{s.error}</div> : null}
                </td>
                <td>
                  <State state={s.state} />
                </td>
                <td class="mono small">{s.size}</td>
                <td>{s.idleMinutes ? `${s.idleMinutes} min` : "never"}</td>
                <td>{s.state === "running" ? ago(s.startedAt) : s.stoppedAt ? `stopped ${ago(s.stoppedAt)}` : "–"}</td>
                <td>{ago(s.createdAt)}</td>
                {write ? (
                  <td class="acts">
                    <Lifecycle s={s} onDone={list.reload} onError={setError} />
                  </td>
                ) : null}
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

// Stop or resume, and delete: on the list and on a sandbox's own page.
export function Lifecycle(props: { s: Sandbox; onDone: () => unknown; onError: (e: string | null) => void; deleted?: () => void }) {
  const { s } = props;
  const ref = s.name ?? s.id;
  const run = (fn: () => Promise<unknown>) => async () => {
    await fn();
    await props.onDone();
  };
  return (
    <span class="row">
      {s.state === "running" ? (
        <Act run={run(() => post(`${sb(s.id)}/stop`))} onError={props.onError} busy="Stopping…">
          Stop
        </Act>
      ) : s.state === "stopped" || s.state === "error" ? (
        <Act run={run(() => post(`${sb(s.id)}/resume`))} onError={props.onError} busy="Starting…">
          Resume
        </Act>
      ) : null}
      <Act
        run={run(async () => {
          await del(sb(s.id));
          props.deleted?.();
        })}
        onError={props.onError}
        kind="danger"
        confirm={`Delete ${ref}? Its disk and saves of it as it is now go for good; named saves made from it stay.`}
        busy="Deleting…"
      >
        Delete
      </Act>
    </span>
  );
}

function NewSandbox(props: { onMade: (s: Sandbox) => void }) {
  const saves = usePoll(() => get<Save[]>("/v1/saves").catch(() => [] as Save[]), []);
  const [name, setName] = useState("");
  const [size, setSize] = useState("standard-2");
  const [idle, setIdle] = useState("30");
  const [from, setFrom] = useState("");
  const [error, setError] = useState<string | null>(null);
  const make = async () => {
    const body = { ...(name ? { name } : {}), size, idleMinutes: Number(idle) };
    if (from) {
      const made = await post<Sandbox[]>(`/v1/saves/${encodeURIComponent(from)}/sandboxes`, body);
      props.onMade(made[0]!);
    } else props.onMade(await post<Sandbox>("/v1/sandboxes", body));
  };
  return (
    <section class="card">
      <div class="form grid">
        <label>
          Name (optional)
          <input value={name} placeholder="web" pattern="[a-z][a-z0-9-]{0,30}[a-z0-9]" onInput={(e) => setName(e.currentTarget.value.trim())} />
        </label>
        <label>
          Size
          <select value={size} onChange={(e) => setSize(e.currentTarget.value)}>
            {SIZES.map((x) => (
              <option value={x} key={x}>
                {x}: {SIZE_NOTE[x]}
              </option>
            ))}
          </select>
        </label>
        <label>
          Stop after idle (minutes; 0 for never)
          <input type="number" min="0" max="360" value={idle} onInput={(e) => setIdle(e.currentTarget.value)} />
        </label>
        <label>
          Start from
          <select value={from} onChange={(e) => setFrom(e.currentTarget.value)}>
            <option value="">a fresh image</option>
            {(saves.data ?? []).map((v) => (
              <option value={v.name} key={v.name}>
                the save {v.name} ({bytes(v.bytes)})
              </option>
            ))}
          </select>
        </label>
      </div>
      <Act run={make} onError={setError} kind="primary" busy="Starting… (about 10 s)">
        Start it
      </Act>
      <Problem error={error} />
    </section>
  );
}
