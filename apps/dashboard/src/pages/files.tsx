// The files tab: browse the sandbox's files (from the home directory, or as
// root), download a file or a directory as tar.gz, upload by dropping files,
// and delete. Names and contents only ever show as text: downloads are
// attachments, never pages of this host.
import type { FileInfo, Me, Sandbox } from "@cloudex/schema";
import { useState } from "preact/hooks";
import { del, get, sb, upload } from "../api";
import { navigate, useLocation } from "../router";
import { Problem, bytes, usePoll, when } from "../ui";
import { canWrite } from "./sandboxes";

// Cloudflare's limit for a request body on most plans.
const MAX_UPLOAD = 100 * 1024 * 1024;

const join = (dir: string, name: string) => (dir.endsWith("/") ? dir + name : `${dir}/${name}`);
const parent = (p: string) => p.replace(/\/[^/]+\/?$/, "") || "/";

export function Files(props: { box: Sandbox; me: Me }) {
  const { query } = useLocation();
  const path = query.get("path") || "~";
  const root = query.get("root") === "1";
  const ref = props.box.name ?? props.box.id;
  const write = canWrite(props.me);
  const q = (p: string) => `path=${encodeURIComponent(p)}${root ? "&root=1" : ""}`;
  const info = usePoll(() => get<FileInfo>(`${sb(props.box.id)}/files?${q(path)}`), [props.box.id, path, root]);
  const [error, setError] = useState<string | null>(null);
  const [progress, setProgress] = useState<string | null>(null);
  const [over, setOver] = useState(false);

  const go = (p: string, asRoot = root) => navigate(`/s/${encodeURIComponent(ref)}/files?path=${encodeURIComponent(p)}${asRoot ? "&root=1" : ""}`);
  const href = (kind: "content" | "archive", p: string) => `${sb(props.box.id)}/files/${kind}?${q(p)}`;

  const dir = info.data?.type === "dir" ? info.data.path : null;
  const sendFiles = async (files: FileList | File[]) => {
    if (!dir) return;
    setError(null);
    for (const f of [...files]) {
      if (f.size > MAX_UPLOAD) {
        setError(`${f.name} is ${bytes(f.size)}; uploads here go up to 100 MB. Use cloudex cp for bigger files.`);
        continue;
      }
      try {
        await upload(`${sb(props.box.id)}/files/content?${q(join(dir, f.name))}`, f, (sent, total) =>
          setProgress(`Uploading ${f.name}: ${Math.round((100 * sent) / (total || 1))}%`),
        );
      } catch (e) {
        setError(`${f.name}: ${e instanceof Error ? e.message : String(e)}`);
      }
    }
    setProgress(null);
    await info.reload();
  };

  const crumbs = (info.data?.path ?? "").split("/").filter(Boolean);
  return (
    <section
      class={`files ${over ? "drop" : ""}`}
      onDragOver={(e) => {
        if (!write || !dir) return;
        e.preventDefault();
        setOver(true);
      }}
      onDragLeave={() => setOver(false)}
      onDrop={(e) => {
        e.preventDefault();
        setOver(false);
        if (write && e.dataTransfer?.files.length) void sendFiles(e.dataTransfer.files);
      }}
    >
      <div class="head sub">
        <h2 class="crumbs mono">
          <button type="button" class="link" onClick={() => go("/")}>
            /
          </button>
          {crumbs.map((c, i) => (
            <span key={i}>
              <button type="button" class="link" onClick={() => go("/" + crumbs.slice(0, i + 1).join("/"))}>
                {c}
              </button>
              {i < crumbs.length - 1 ? "/" : ""}
            </span>
          ))}
        </h2>
        <button type="button" class="btn btn-quiet" onClick={() => go("~")}>
          Home
        </button>
        <label class="check small">
          <input type="checkbox" checked={root} onChange={(e) => go(path, e.currentTarget.checked)} />
          As root
        </label>
        {dir ? (
          <a class="btn btn-quiet" href={href("archive", dir)} download>
            Download as tar.gz
          </a>
        ) : null}
        {write && dir ? (
          <label class="btn">
            Upload
            <input
              type="file"
              multiple
              hidden
              onChange={(e) => {
                const f = e.currentTarget.files;
                if (f?.length) void sendFiles(f);
                e.currentTarget.value = "";
              }}
            />
          </label>
        ) : null}
      </div>
      {progress ? <p class="note">{progress}</p> : null}
      <Problem error={error ?? info.error} />
      {info.data?.type === "dir" ? (
        <>
          {write ? <p class="muted small">Drop files here to upload them into this directory.</p> : null}
          <table class="list compact">
            <thead>
              <tr>
                <th>Name</th>
                <th>Size</th>
                <th>Mode</th>
                <th>Changed</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {info.data.path !== "/" ? (
                <tr>
                  <td colSpan={5}>
                    <button type="button" class="link mono" onClick={() => go(parent(info.data!.path))}>
                      ..
                    </button>
                  </td>
                </tr>
              ) : null}
              {(info.data.entries ?? []).map((e) => {
                const p = join(info.data!.path, e.name);
                return (
                  <tr key={e.name}>
                    <td class="mono">
                      {e.type === "dir" ? (
                        <button type="button" class="link" onClick={() => go(p)}>
                          {e.name}/
                        </button>
                      ) : e.type === "file" ? (
                        <a href={href("content", p)} download={e.name}>
                          {e.name}
                        </a>
                      ) : e.type === "symlink" ? (
                        <button type="button" class="link" onClick={() => go(p)} title={`→ ${e.target ?? "?"}`}>
                          {e.name} → {e.target ?? "?"}
                        </button>
                      ) : (
                        <span>{e.name}</span>
                      )}
                    </td>
                    <td class="small">{e.type === "dir" ? "" : bytes(e.size)}</td>
                    <td class="mono small">{e.mode}</td>
                    <td class="small" title={when(e.mtime)}>
                      {new Date(e.mtime).toLocaleDateString()}
                    </td>
                    <td class="acts">
                      {e.type === "dir" ? (
                        <a class="btn btn-quiet" href={href("archive", p)} download>
                          tar.gz
                        </a>
                      ) : null}
                      {write ? (
                        <button
                          type="button"
                          class="btn btn-quiet danger-text"
                          onClick={async () => {
                            if (!confirm(e.type === "dir" ? `Delete ${p} and everything in it?` : `Delete ${p}?`)) return;
                            try {
                              await del(`${sb(props.box.id)}/files?${q(p)}${e.type === "dir" ? "&recursive=1" : ""}`);
                              await info.reload();
                            } catch (x) {
                              setError(x instanceof Error ? x.message : String(x));
                            }
                          }}
                        >
                          Delete
                        </button>
                      ) : null}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          {info.data.truncated ? <p class="muted small">Only the first 10,000 entries show.</p> : null}
        </>
      ) : info.data ? (
        <section class="card">
          <dl class="facts">
            <dt>Path</dt>
            <dd class="mono">{info.data.path}</dd>
            <dt>Type</dt>
            <dd>{info.data.type}</dd>
            <dt>Size</dt>
            <dd>{bytes(info.data.size)}</dd>
            <dt>Mode</dt>
            <dd class="mono">{info.data.mode}</dd>
            <dt>Changed</dt>
            <dd>{when(info.data.mtime)}</dd>
          </dl>
          <div class="row">
            {info.data.type === "file" ? (
              <a class="btn btn-primary" href={href("content", info.data.path)} download>
                Download
              </a>
            ) : null}
            <button type="button" class="btn" onClick={() => go(parent(info.data!.path))}>
              The directory it's in
            </button>
          </div>
        </section>
      ) : null}
    </section>
  );
}
