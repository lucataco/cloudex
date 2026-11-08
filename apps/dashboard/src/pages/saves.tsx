// /saves: named saves, new sandboxes from one, and deleting one.
import type { Me, Sandbox, Save } from "@cloudex/schema";
import { Fragment } from "preact";
import { useState } from "preact/hooks";
import { del, get, post } from "../api";
import { navigate } from "../router";
import { Act, Loading, Problem, SIZES, ago, bytes, usePoll } from "../ui";
import { canWrite } from "./sandboxes";

export function Saves(props: { me: Me }) {
  const list = usePoll(() => get<Save[]>("/v1/saves"), []);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState<string | null>(null);
  const write = canWrite(props.me);
  const noBucket = /need an R2 bucket/.test(list.error ?? "");
  return (
    <>
      <div class="head">
        <h1>Saves</h1>
      </div>
      <p class="muted">A named save is a copy of a sandbox's disk to start new sandboxes from. Make one on a sandbox's page, or with cloudex save.</p>
      {noBucket ? <p class="note">{list.error}</p> : <Problem error={error ?? list.error} />}
      {!list.data ? (
        noBucket ? null : (
          <Loading />
        )
      ) : list.data.length === 0 ? (
        <p class="muted">No named saves yet.</p>
      ) : (
        <table class="list">
          <thead>
            <tr>
              <th>Name</th>
              <th>From</th>
              <th>Files</th>
              <th>Size</th>
              <th>Made</th>
              {write ? <th /> : null}
            </tr>
          </thead>
          <tbody>
            {list.data.map((v) => (
              <Fragment key={v.name}>
                <tr>
                  <td class="strong">
                    {v.name}
                    <span class="id">{v.id}</span>
                  </td>
                  <td class="mono small">
                    {v.sandbox}
                    {v.kind === "home" ? " (home only)" : ""}
                  </td>
                  <td>{v.files.toLocaleString()}</td>
                  <td>
                    {bytes(v.bytes)} <span class="muted small">of {bytes(v.rawBytes)}</span>
                  </td>
                  <td>{ago(v.createdAt)}</td>
                  {write ? (
                    <td class="acts">
                      <button type="button" class="btn" onClick={() => setOpen(open === v.name ? null : v.name)}>
                        New sandbox
                      </button>
                      {props.me.ownOnly ? null : (
                        <Act
                          run={async () => {
                            await del(`/v1/saves/${encodeURIComponent(v.name)}`);
                            await list.reload();
                          }}
                          onError={setError}
                          kind="danger"
                          confirm={`Delete the save ${v.name}? Sandboxes started from it keep running.`}
                        >
                          Delete
                        </Act>
                      )}
                    </td>
                  ) : null}
                </tr>
                {open === v.name ? (
                  <tr>
                    <td colSpan={6}>
                      <FromSave save={v} />
                    </td>
                  </tr>
                ) : null}
              </Fragment>
            ))}
          </tbody>
        </table>
      )}
    </>
  );
}

function FromSave(props: { save: Save }) {
  const [name, setName] = useState("");
  const [count, setCount] = useState("1");
  const [size, setSize] = useState(props.save.size);
  const [error, setError] = useState<string | null>(null);
  const start = async () => {
    const made = await post<Sandbox[]>(`/v1/saves/${encodeURIComponent(props.save.name)}/sandboxes`, {
      ...(name ? { name } : {}),
      count: Number(count),
      size,
    });
    navigate(made.length === 1 ? `/s/${made[0]!.name ?? made[0]!.id}` : "/");
  };
  return (
    <div class="form grid inset">
      <label>
        Name (optional; with a count, name-1, name-2…)
        <input value={name} onInput={(e) => setName(e.currentTarget.value.trim())} />
      </label>
      <label>
        How many
        <input type="number" min="1" max="10" value={count} onInput={(e) => setCount(e.currentTarget.value)} />
      </label>
      <label>
        Size
        <select value={size} onChange={(e) => setSize(e.currentTarget.value as Save["size"])}>
          {SIZES.map((x) => (
            <option key={x}>{x}</option>
          ))}
        </select>
      </label>
      <div>
        <Act run={start} onError={setError} kind="primary" busy="Starting…">
          Start
        </Act>
      </div>
      <Problem error={error} />
    </div>
  );
}
