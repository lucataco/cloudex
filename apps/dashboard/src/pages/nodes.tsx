// /nodes, for admins: your own Docker hosts that run sandboxes. Add one (its
// install command is shown once, here), see each one's state and host, and
// remove one.
//
// The list comes from the deployment's index (GET /v1/nodes?live=false), as
// each node's object last wrote it, every 30 seconds: asking the objects
// themselves on every poll would keep them awake, and an object kept busy
// drops its node's connection a few minutes after each deploy. A node's
// object writes online at its agent's hello and offline when its connection
// goes (and checks every 3 minutes that it's still answering), so the index
// is current to within that.
//
// Everything about a node but its id comes from its agent, or from whoever
// named it: it is shown as text only, as everything from outside is in the
// dashboard.
import type { NewNode, Node } from "@cloudex/schema";
import { useState } from "preact/hooks";
import { del, get, post } from "../api";
import { Act, Cmd, Loading, Problem, ago, nodeIsolationText, until, usePoll } from "../ui";

const POLL_MS = 30_000;

export function Nodes() {
  const list = usePoll(() => get<Node[]>("/v1/nodes?live=false"), [], POLL_MS);
  const [error, setError] = useState<string | null>(null);
  const [made, setMade] = useState<NewNode | null>(null);
  return (
    <>
      <div class="head">
        <h1>Nodes</h1>
      </div>
      <p class="muted small">
        A node is a Linux host of yours, with Docker, that runs sandboxes for this deployment. Its agent dials out and stays connected; the host needs no open
        port. Pick a node under Where when you make a sandbox, or run cloudex new --on &lt;node&gt;.
      </p>
      <AddNode
        onAdded={async (n) => {
          setMade(n);
          await list.reload();
        }}
      />
      {made ? <Installer made={made} onDone={() => setMade(null)} /> : null}
      <Problem error={error ?? list.error} />
      {!list.data ? (
        <Loading />
      ) : list.data.length === 0 ? (
        <p class="muted">No nodes yet.</p>
      ) : (
        <table class="list">
          <thead>
            <tr>
              <th>Name</th>
              <th>State</th>
              <th>Isolation</th>
              <th>Host</th>
              <th>Sandboxes</th>
              <th>Seen</th>
              <th />
            </tr>
          </thead>
          <tbody>
            {list.data.map((n) => (
              <tr key={n.id}>
                <td>
                  <span class="strong">{n.name}</span>
                  <span class="id">{n.id}</span>
                  {n.version ? <div class="small muted">agent {n.version}</div> : null}
                </td>
                <td>
                  <span class="state">
                    <i class={`led led-${n.state === "online" ? "run" : n.state === "revoked" ? "bad" : n.state === "pending" ? "wait" : "off"}`} aria-hidden="true" />
                    {n.state}
                  </span>
                </td>
                <td class="small">
                  {nodeIsolationText(n)}
                  {n.isolation?.failed?.length ? <div class="small problem-text">{n.isolation.failed.slice(0, 3).join("; ")}</div> : null}
                </td>
                <td class="small mono">
                  {[n.arch, n.cpus ? `${n.cpus} CPUs` : null, n.memoryMb ? `${(n.memoryMb / 1024).toFixed(1)} GiB` : null, n.diskFreeGb !== null ? `${Math.round(n.diskFreeGb)} GB free` : null]
                    .filter(Boolean)
                    .join(", ") || "–"}
                </td>
                <td>{n.machines}</td>
                <td>{n.state === "online" ? "now" : n.state === "pending" ? "never" : ago(n.lastSeenAt)}</td>
                <td class="acts">
                  <RemoveNode n={n} onDone={list.reload} onError={setError} />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function AddNode(props: { onAdded: (n: NewNode) => unknown }) {
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  return (
    <section class="card">
      <div class="row">
        <input value={name} placeholder="box" pattern="[a-z][a-z0-9-]{0,30}[a-z0-9]" aria-label="Node name" onInput={(e) => setName(e.currentTarget.value.trim())} />
        <Act
          run={async () => {
            await props.onAdded(await post<NewNode>("/v1/nodes", { name }));
            setName("");
          }}
          onError={setError}
          kind="primary"
          disabled={!name}
          busy="Adding…"
        >
          Add node
        </Act>
      </div>
      <Problem error={error} />
    </section>
  );
}

// The install command, shown once: its join token works once, until it
// expires, and nothing shows it again.
function Installer(props: { made: NewNode; onDone: () => void }) {
  const { node, install, expiresAt } = props.made;
  return (
    <section class="card">
      <h2>Install the agent on {node.name}</h2>
      <p class="small">
        On the host, as root, with Docker running, run this. Its token works once, and expires {until(expiresAt)}; it isn't shown again (adding the same name
        again gives a new one).
      </p>
      <Cmd text={install} />
      <p class="small muted">
        The installer sets up a firewall for the node's machines, starts the agent and checks the isolation; the node then shows online here. If the host has no
        Sysbox, machines on it run under plain Docker: root in a machine is root on the host if code escapes the container, and from there its LAN and tailnet
        are in reach. Use such a node only for code you trust.
      </p>
      <button type="button" class="btn btn-quiet" onClick={props.onDone}>
        Done
      </button>
    </section>
  );
}

function RemoveNode(props: { n: Node; onDone: () => unknown; onError: (e: string | null) => void }) {
  const { n } = props;
  const forget = n.state === "revoked" || n.state === "pending";
  const confirmText = forget
    ? `Forget ${n.name}?`
    : n.machines > 0
      ? `Remove ${n.name}? It has ${n.machines} sandbox${n.machines === 1 ? "" : "es"}: ${n.state === "online" ? "their machines are deleted with it" : "their machines stay on the host, which can't be reached any more"}, and they stay listed until you delete them. Its agent is disconnected and must join again with a new token.`
      : `Remove ${n.name}? Its agent is disconnected and must join again with a new token.`;
  return (
    <Act
      run={async () => {
        await del(`/v1/nodes/${encodeURIComponent(n.id)}${!forget && n.machines > 0 ? "?force=1" : ""}`);
        await props.onDone();
      }}
      onError={props.onError}
      kind="danger"
      confirm={confirmText}
      busy="Removing…"
    >
      {forget ? "Forget" : "Remove"}
    </Act>
  );
}
