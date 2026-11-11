// /: the sandboxes, refreshed every 5 seconds, and a new one.
import type { Me, Node, Sandbox, Save } from "@cloudex/schema";
import { useState } from "preact/hooks";
import { del, get, post, sb } from "../api";
import { Link, navigate } from "../router";
import { Act, Loading, Problem, SIZES, SIZE_NOTE, State, ago, bytes, nodeIsolationText, onNode, usePoll, whereText } from "../ui";

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
              <th>Where</th>
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
                <td class={onNode(s) && !s.placement?.node?.online ? "small problem-text" : "small"}>{whereText(s)}</td>
                <td class="mono small">{onNode(s) ? "–" : s.size}</td>
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
  const nodeOffline = onNode(s) && !s.placement?.node?.online && s.placement?.node?.state !== "revoked" && s.placement?.node?.state !== null;
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
          // On a node that's offline: deleted now, and the node removes the
          // machine when it's back (the confirmation says so).
          await del(sb(s.id) + (nodeOffline ? "?force=1" : ""));
          props.deleted?.();
        })}
        onError={props.onError}
        kind="danger"
        confirm={
          nodeOffline
            ? `Delete ${ref}? Its node ${s.placement?.node?.name ?? ""} is offline: ${ref} goes now, and the node removes its machine and disk when it's back.`
            : `Delete ${ref}? Its disk and saves of it as it is now go for good; named saves made from it stay.`
        }
        busy="Deleting…"
      >
        Delete
      </Act>
    </span>
  );
}

function NewSandbox(props: { onMade: (s: Sandbox) => void }) {
  const saves = usePoll(() => get<Save[]>("/v1/saves").catch(() => [] as Save[]), []);
  // From the index, as the Nodes page reads them (pages/nodes.tsx says why).
  const nodes = usePoll(() => get<Node[]>("/v1/nodes?live=false").catch(() => [] as Node[]), []);
  const [name, setName] = useState("");
  const [size, setSize] = useState("standard-2");
  const [idle, setIdle] = useState("30");
  const [from, setFrom] = useState("");
  const [where, setWhere] = useState("");
  const [cpus, setCpus] = useState("");
  const [memory, setMemory] = useState("");
  const [error, setError] = useState<string | null>(null);
  const usable = (nodes.data ?? []).filter((n) => n.state === "online" || n.state === "offline");
  const node = usable.find((n) => n.id === where);
  const make = async () => {
    const body = {
      ...(name ? { name } : {}),
      size,
      idleMinutes: Number(idle),
      ...(node ? { node: node.id, ...(cpus ? { cpus: Number(cpus) } : {}), ...(memory ? { memoryMb: Math.round(Number(memory) * 1024) } : {}) } : {}),
    };
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
        {usable.length ? (
          <label>
            Where
            <select value={where} onChange={(e) => (setWhere(e.currentTarget.value), setFrom(""))}>
              <option value="">Cloudflare</option>
              {usable.map((n) => (
                <option value={n.id} key={n.id}>
                  node {n.name}
                  {n.state === "online" ? "" : " (offline)"}: {nodeIsolationText(n)}
                </option>
              ))}
            </select>
          </label>
        ) : null}
        {node ? (
          <>
            <label>
              CPUs (default the size's, up to {node.cpus ?? "?"})
              <input type="number" min="0.25" step="0.25" max={node.cpus ?? undefined} value={cpus} onInput={(e) => setCpus(e.currentTarget.value)} />
            </label>
            <label>
              Memory in GiB (default the size's, at most half the node's)
              <input type="number" min="0.5" step="0.5" value={memory} onInput={(e) => setMemory(e.currentTarget.value)} />
            </label>
          </>
        ) : null}
        <label>
          Start from
          <select value={from} disabled={!!node} onChange={(e) => setFrom(e.currentTarget.value)}>
            <option value="">a fresh image</option>
            {(saves.data ?? []).map((v) => (
              <option value={v.name} key={v.name}>
                the save {v.name} ({bytes(v.bytes)})
              </option>
            ))}
          </select>
        </label>
      </div>
      {node && (node.isolation?.unisolated || node.runtime === "docker") ? (
        <p class="note small">
          Machines on {node.name} run under {node.isolation?.unisolated ? "no isolation (it joined with --unisolated, for testing only)" : "plain Docker"}: root in a
          machine is root on that host if code escapes the container. Use it only for code you trust.
        </p>
      ) : null}
      {node ? <p class="muted small">On a node, a stopped sandbox keeps its disk there. Forks, named saves and agents aren't available on nodes yet.</p> : null}
      <Act run={make} onError={setError} kind="primary" busy="Starting… (about 10 s)">
        Start it
      </Act>
      <Problem error={error} />
    </section>
  );
}
