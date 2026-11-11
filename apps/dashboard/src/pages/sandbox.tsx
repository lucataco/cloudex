// /s/:ref and its tabs: overview, terminal, desktop, agents, commands, files.
import type { HostLink, Me, Sandbox, SandboxSettings } from "@cloudex/schema";
import { useState } from "preact/hooks";
import { del, get, patch, post, sb } from "../api";
import { Link, match, navigate, useLocation } from "../router";
import { Act, Cmd, Copy, ISOLATION_TEXT, Loading, Problem, State, ago, bytes, limitsText, onNode, usePoll, when, whereText } from "../ui";
import { Agents } from "./agents";
import { Commands } from "./commands";
import { Desktop } from "./desktop";
import { Files } from "./files";
import { Lifecycle, canWrite } from "./sandboxes";
import { Terminals } from "./terminal";

const TABS = [
  ["", "Overview"],
  ["terminal", "Terminal"],
  ["desktop", "Desktop"],
  ["agents", "Agents"],
  ["commands", "Commands"],
  ["files", "Files"],
] as const;

export function SandboxPage(props: { ref_: string; me: Me }) {
  const { path } = useLocation();
  const ref = props.ref_;
  const s = usePoll(() => get<Sandbox>(sb(ref)), [ref], 5000);
  const [error, setError] = useState<string | null>(null);
  const base = `/s/${encodeURIComponent(ref)}`;
  const tab = match("/s/:ref/:tab/*", path)?.tab ?? "";
  const write = canWrite(props.me);

  if (!s.data) return s.error ? <Problem error={s.error} /> : <Loading />;
  const box = s.data;
  const running = box.state === "running";
  const tabs = TABS.filter(([t]) => write || (t !== "terminal" && t !== "desktop"));
  return (
    <>
      <div class="head">
        <h1>
          {box.name ?? box.id}
          {box.name ? <span class="id">{box.id}</span> : null}
        </h1>
        <State state={box.state} />
        {write ? <Lifecycle s={box} onDone={s.reload} onError={setError} deleted={() => navigate("/")} /> : null}
      </div>
      <Problem error={error ?? box.error} />
      {box.notice ? <p class="note">{box.notice}</p> : null}
      <nav class="tabs">
        {tabs.map(([t, label]) => (
          <Link href={t ? `${base}/${t}` : base} class={tab === t ? "on" : ""} key={t}>
            {label}
          </Link>
        ))}
      </nav>
      {/* The terminal stays mounted while another tab shows, so its shells stay connected. */}
      {write ? (
        <div hidden={tab !== "terminal"}>
          <Terminals box={box} active={tab === "terminal"} onChange={s.reload} />
        </div>
      ) : null}
      {tab === "" ? <Overview box={box} me={props.me} reload={s.reload} /> : null}
      {tab !== "" && tab !== "terminal" && !running ? <NotRunning box={box} reload={s.reload} /> : null}
      {running && tab === "desktop" ? <Desktop box={box} /> : null}
      {running && tab === "agents" && onNode(box) ? (
        <section class="card">
          <p>Agents need the deployment's provider keys, which don't reach sandboxes on nodes yet. Start agents in a sandbox on Cloudflare.</p>
        </section>
      ) : null}
      {running && tab === "agents" && !onNode(box) ? <Agents box={box} me={props.me} /> : null}
      {running && tab === "commands" ? <Commands box={box} me={props.me} /> : null}
      {running && tab === "files" ? <Files box={box} me={props.me} /> : null}
    </>
  );
}

export function NotRunning(props: { box: Sandbox; reload: () => unknown }) {
  const [error, setError] = useState<string | null>(null);
  return (
    <section class="card">
      <p>
        {props.box.name ?? props.box.id} is {props.box.state}.
        {props.box.state === "stopped" ? " Resume it to use this." : ""}
      </p>
      {props.box.state === "stopped" || props.box.state === "error" ? (
        <Act
          run={async () => {
            await post(`${sb(props.box.id)}/resume`);
            await props.reload();
          }}
          onError={setError}
          kind="primary"
          busy="Starting…"
        >
          Resume
        </Act>
      ) : null}
      <Problem error={error} />
    </section>
  );
}

function Overview(props: { box: Sandbox; me: Me; reload: () => unknown }) {
  const { box } = props;
  const write = canWrite(props.me);
  const settings = usePoll(() => get<SandboxSettings>(`${sb(box.id)}/settings`), [box.id]);
  const ref = box.name ?? box.id;
  return (
    <div class="cols">
      <section class="card">
        <h2>About</h2>
        <dl class="facts">
          <dt>Id</dt>
          <dd class="mono">{box.id}</dd>
          <dt>Runs on</dt>
          <dd class={onNode(box) && !box.placement?.node?.online ? "problem-text" : ""}>
            {onNode(box) ? `node ${whereText(box)}` : "Cloudflare"}
            {onNode(box) && box.placement?.node?.state === "offline" ? `, last seen ${ago(box.placement.node.lastSeenAt)}` : ""}
          </dd>
          {box.isolation ? (
            <>
              <dt>Isolation</dt>
              <dd>{ISOLATION_TEXT[box.isolation] ?? box.isolation}</dd>
            </>
          ) : null}
          {onNode(box) ? (
            <>
              <dt>Machine</dt>
              <dd class="mono">{limitsText(box)}</dd>
            </>
          ) : (
            <>
              <dt>Size</dt>
              <dd class="mono">{box.size}</dd>
            </>
          )}
          <dt>Made</dt>
          <dd title={when(box.createdAt)}>{ago(box.createdAt)}</dd>
          <dt>{box.state === "running" ? "Started" : "Stopped"}</dt>
          <dd>{ago(box.state === "running" ? box.startedAt : box.stoppedAt)}</dd>
          {onNode(box) ? null : (
            <>
              <dt>Last save</dt>
              <dd>{box.savedAt ? `${ago(box.savedAt)}, ${bytes(box.savedBytes)}` : "–"}</dd>
            </>
          )}
          {box.backupAt ? (
            <>
              <dt>Backup</dt>
              <dd>
                {ago(box.backupAt)}, {bytes(box.backupBytes)}
              </dd>
            </>
          ) : null}
          {box.backupError ? (
            <>
              <dt>Backup failed</dt>
              <dd class="problem-text">{box.backupError}</dd>
            </>
          ) : null}
          {box.forkedFrom ? (
            <>
              <dt>Forked from</dt>
              <dd class="mono">{box.forkedFrom}</dd>
            </>
          ) : null}
          {box.fromSave ? (
            <>
              <dt>Started from</dt>
              <dd>the save {box.fromSave}</dd>
            </>
          ) : null}
          <dt>Provider keys</dt>
          <dd>{box.providerKeys?.length ? box.providerKeys.join(", ") : "none"}</dd>
        </dl>
        {write ? <Edit box={box} reload={props.reload} /> : null}
      </section>
      <section class="card">
        <h2>From a terminal</h2>
        <Cmd text={`cloudex ssh ${ref}`} />
        <Cmd text={`cloudex exec ${ref} -- uname -a`} />
        <h2>Settings</h2>
        <p class="muted small">Names only: values never come back from the API. Change them with cloudex set {ref}.</p>
        {settings.data ? (
          <dl class="facts">
            <dt>Environment</dt>
            <dd class="mono small">{settings.data.env.length ? settings.data.env.join(" ") : "none"}</dd>
            <dt>Files</dt>
            <dd class="mono small">{settings.data.files.length ? settings.data.files.join(" ") : "none"}</dd>
            <dt>Repo</dt>
            <dd class="mono small">
              {settings.data.repo ? `${settings.data.repo.url}${settings.data.repo.branch ? `#${settings.data.repo.branch}` : ""} → ${settings.data.repo.dir}` : "none"}
            </dd>
          </dl>
        ) : (
          <Problem error={settings.error} />
        )}
      </section>
      {write ? <Links box={box} /> : null}
      {write && !onNode(box) ? <ForkAndSave box={box} me={props.me} /> : null}
      {write && onNode(box) ? (
        <section class="card">
          <h2>Fork and save</h2>
          <p class="muted small">Forks and named saves of a sandbox on a node aren't available yet. Copy what you need with cloudex cp.</p>
        </section>
      ) : null}
    </div>
  );
}

function Edit(props: { box: Sandbox; reload: () => unknown }) {
  const [name, setName] = useState(props.box.name ?? "");
  const [idle, setIdle] = useState(String(props.box.idleMinutes));
  const [error, setError] = useState<string | null>(null);
  const changed = name !== (props.box.name ?? "") || idle !== String(props.box.idleMinutes);
  const save = async () => {
    const body: Record<string, unknown> = {};
    if (name && name !== props.box.name) body.name = name;
    if (idle !== String(props.box.idleMinutes)) body.idleMinutes = Number(idle);
    await patch(sb(props.box.id), body);
    if (body.name) navigate(`/s/${name}`, true);
    await props.reload();
  };
  return (
    <div class="form grid">
      <label>
        Name
        <input value={name} onInput={(e) => setName(e.currentTarget.value.trim())} />
      </label>
      <label>
        Stop after idle (minutes; 0 for never)
        <input type="number" min="0" max="360" value={idle} onInput={(e) => setIdle(e.currentTarget.value)} />
      </label>
      <div>
        <Act run={save} onError={setError} disabled={!changed}>
          Save changes
        </Act>
      </div>
      <Problem error={error} />
    </div>
  );
}

// Preview links open on the preview domain, in a new tab: never on this
// host, where the dashboard's cookie works.
function Links(props: { box: Sandbox }) {
  const links = usePoll(() => get<HostLink[]>(`${sb(props.box.id)}/hosts`), [props.box.id]);
  const [port, setPort] = useState("3000");
  const [error, setError] = useState<string | null>(null);
  const noZone = /preview links need a domain/.test(links.error ?? "");
  return (
    <section class="card">
      <h2>Preview links</h2>
      {noZone ? (
        <p class="muted small">
          This deployment has no preview domain. <span class="mono">cloudex deploy --preview-domain example.com</span> adds one; until then, cloudex proxy reaches a port.
        </p>
      ) : (
        <>
          <p class="muted small">Anyone with a link reaches that port's server, until it's revoked.</p>
          <Problem error={error ?? links.error} />
          {links.data?.length ? (
            <table class="list compact">
              <tbody>
                {links.data.map((l) => (
                  <tr key={l.port}>
                    <td class="mono">{l.port}</td>
                    <td class="mono small wrap">
                      <a href={l.url} target="_blank" rel="noopener noreferrer">
                        {l.url}
                      </a>
                    </td>
                    <td class="acts">
                      <Copy text={l.url} />
                      <Act
                        run={async () => {
                          await del(`${sb(props.box.id)}/hosts/${l.port}`);
                          await links.reload();
                        }}
                        onError={setError}
                        kind="danger"
                      >
                        Revoke
                      </Act>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : links.data ? (
            <p class="muted small">No links yet.</p>
          ) : null}
          <div class="row">
            <input type="number" min="1" max="65535" value={port} class="narrow-input" onInput={(e) => setPort(e.currentTarget.value)} aria-label="Port" />
            <Act
              run={async () => {
                await post(`${sb(props.box.id)}/hosts`, { port: Number(port) });
                await links.reload();
              }}
              onError={setError}
            >
              Link to this port
            </Act>
          </div>
        </>
      )}
    </section>
  );
}

function ForkAndSave(props: { box: Sandbox; me: Me }) {
  const [forkName, setForkName] = useState("");
  const [saveName, setSaveName] = useState(props.box.name ?? "");
  const [error, setError] = useState<string | null>(null);
  const [note, setNote] = useState<string | null>(null);
  return (
    <section class="card">
      <h2>Fork and save</h2>
      <p class="muted small">A fork is a new sandbox with a copy of this one's disk. A named save is a copy kept to start new sandboxes from.</p>
      <div class="row">
        <input placeholder="name of the fork (optional)" value={forkName} onInput={(e) => setForkName(e.currentTarget.value.trim())} />
        <Act
          run={async () => {
            const made = await post<Sandbox[]>(`${sb(props.box.id)}/fork`, forkName ? { name: forkName } : {});
            navigate(`/s/${made[0]!.name ?? made[0]!.id}`);
          }}
          onError={setError}
          busy="Forking…"
          disabled={props.box.state !== "running"}
        >
          Fork
        </Act>
      </div>
      {props.me.ownOnly ? null : (
        <div class="row">
          <input placeholder="save name" value={saveName} onInput={(e) => setSaveName(e.currentTarget.value.trim())} />
          <Act
            run={async () => {
              const v = await post<{ name: string; bytes: number; replaced?: boolean }>(`${sb(props.box.id)}/saves`, { name: saveName });
              setNote(`Saved as ${v.name} (${bytes(v.bytes)})${v.replaced ? ", replacing the earlier save" : ""}.`);
            }}
            onError={setError}
            busy="Saving…"
            disabled={!saveName || props.box.state !== "running"}
          >
            Save
          </Act>
        </div>
      )}
      {note ? <p class="note">{note}</p> : null}
      <Problem error={error} />
    </section>
  );
}
