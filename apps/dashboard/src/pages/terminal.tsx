// The terminal tab: shells in the sandbox over GET /v1/sandboxes/:ref/terminal.
// Each tab is a tmux session (dash-1, dash-2, ...), so when the socket drops,
// as it does when the deployment is updated, the tab reconnects to the same
// shell. Closing a tab ends its session. While the browser tab is visible,
// each open terminal pings once a minute, which counts as activity, so the
// sandbox doesn't stop under someone looking at it.
import type { Sandbox } from "@cloudex/schema";
import { FitAddon } from "@xterm/addon-fit";
import { Terminal } from "@xterm/xterm";
import { useEffect, useRef, useState } from "preact/hooks";
import { post, sb, whoAmI, wsUrl } from "../api";
import { Act, Problem } from "../ui";

type Tab = { id: string; root: boolean };

const store = (box: string) => `cx.terminals.${box}`;
function loadTabs(box: string): Tab[] {
  try {
    const t = JSON.parse(localStorage.getItem(store(box)) ?? "null") as Tab[] | null;
    if (Array.isArray(t) && t.every((x) => /^dash-\d{1,6}(-root)?$/.test(x.id))) return t;
  } catch {}
  return [];
}
function saveTabs(box: string, tabs: Tab[]) {
  try {
    localStorage.setItem(store(box), JSON.stringify(tabs));
  } catch {}
}

export function Terminals(props: { box: Sandbox; active: boolean; onChange: () => unknown }) {
  const box = props.box;
  const [tabs, setTabs] = useState<Tab[]>(() => {
    const t = loadTabs(box.id);
    return t.length ? t : [{ id: "dash-1", root: false }];
  });
  const [current, setCurrent] = useState(tabs[0]?.id ?? "dash-1");
  // No shells until the tab is first opened.
  const [opened, setOpened] = useState(props.active);
  const [error, setError] = useState<string | null>(null);
  useEffect(() => {
    if (props.active) setOpened(true);
  }, [props.active]);
  useEffect(() => saveTabs(box.id, tabs), [tabs]);

  const add = (root: boolean) => {
    const n = Math.max(0, ...tabs.map((t) => Number(/\d+/.exec(t.id)?.[0] ?? 0))) + 1;
    const t = { id: `dash-${n}${root ? "-root" : ""}`, root };
    setTabs([...tabs, t]);
    setCurrent(t.id);
  };
  const close = (id: string) => {
    // The tab's terminal ends its tmux session as it goes.
    window.dispatchEvent(new Event(`cx-kill-${id}`));
    let rest = tabs.filter((t) => t.id !== id);
    if (!rest.length) rest = [{ id: `dash-${Number(/\d+/.exec(id)?.[0] ?? 0) + 1}`, root: false }];
    setTabs(rest);
    if (current === id || !rest.some((t) => t.id === current)) setCurrent(rest[rest.length - 1]!.id);
  };

  if (!opened) return null;
  const running = box.state === "running";
  return (
    <section class="terms">
      <div class="term-tabs">
        {tabs.map((t, i) => (
          <span class={`term-tab ${t.id === current ? "on" : ""}`} key={t.id}>
            <button type="button" onClick={() => setCurrent(t.id)}>
              {t.root ? "root" : "shell"} {i + 1}
            </button>
            <button type="button" class="x" title="Close: ends this shell" aria-label="Close this shell" onClick={() => close(t.id)}>
              ×
            </button>
          </span>
        ))}
        <button type="button" class="btn btn-quiet" onClick={() => add(false)}>
          + shell
        </button>
        <button type="button" class="btn btn-quiet" onClick={() => add(true)}>
          + root
        </button>
        <span class="muted small hint">Hold Shift (⌥ on a Mac) to select text. The mouse wheel scrolls back.</span>
      </div>
      {running ? null : (
        <div class="card">
          <p>
            {box.name ?? box.id} is {box.state}. The shells come back when it's running again; each one starts afresh after a stop.
          </p>
          {box.state === "stopped" || box.state === "error" ? (
            <Act
              run={async () => {
                await post(`${sb(box.id)}/resume`);
                await props.onChange();
              }}
              onError={setError}
              kind="primary"
              busy="Starting…"
            >
              Resume
            </Act>
          ) : null}
          <Problem error={error} />
        </div>
      )}
      {tabs.map((t) => (
        <Term key={t.id} box={box.id} tab={t} visible={props.active && t.id === current} running={running} onLost={props.onChange} onClosed={() => close(t.id)} />
      ))}
    </section>
  );
}

const THEME = {
  background: "#0b0a09",
  foreground: "#f3eee8",
  cursor: "#f38020",
  cursorAccent: "#0b0a09",
  selectionBackground: "#f3802055",
  black: "#15120f",
  brightBlack: "#6e675f",
  red: "#e5534b",
  green: "#1fbf62",
  yellow: "#f2a20c",
  blue: "#5ea1ff",
  magenta: "#c678dd",
  cyan: "#3fc1c9",
  white: "#d8d2cb",
  brightWhite: "#ffffff",
};

type Status = "connecting" | "open" | "reconnecting" | "exited" | "waiting";

function Term(props: { box: string; tab: Tab; visible: boolean; running: boolean; onLost: () => unknown; onClosed: () => void }) {
  const el = useRef<HTMLDivElement>(null);
  const term = useRef<Terminal>();
  const fit = useRef<FitAddon>();
  const ws = useRef<WebSocket | null>(null);
  const [status, setStatus] = useState<Status>("connecting");
  const [exit, setExit] = useState<number | null>(null);
  const state = useRef({ disposed: false, ended: false, failures: 0, timer: 0 as unknown as ReturnType<typeof setTimeout>, running: props.running, killing: false });
  state.current.running = props.running;

  const connect = () => {
    const t = term.current!;
    const st = state.current;
    if (st.disposed || st.ended || !st.running) return;
    clearTimeout(st.timer);
    const q = new URLSearchParams({ session: props.tab.id, cols: String(t.cols), rows: String(t.rows) });
    if (props.tab.root) q.set("root", "1");
    const sock = new WebSocket(wsUrl(`${sb(props.box)}/terminal?${q}`));
    sock.binaryType = "arraybuffer";
    ws.current = sock;
    let opened = false;
    sock.onopen = () => {
      opened = true;
      st.failures = 0;
      // tmux draws the whole screen again on attach.
      t.reset();
      setStatus("open");
      sock.send(JSON.stringify({ resize: [t.cols, t.rows] }));
    };
    sock.onmessage = (e) => {
      if (typeof e.data === "string") {
        try {
          const m = JSON.parse(e.data) as { exit?: number };
          if (typeof m.exit === "number") {
            st.ended = true;
            setExit(m.exit);
            setStatus("exited");
          }
        } catch {}
        return;
      }
      t.write(new Uint8Array(e.data as ArrayBuffer));
    };
    sock.onclose = () => {
      if (ws.current === sock) ws.current = null;
      if (st.disposed || st.ended) return;
      if (!opened) st.failures++;
      setStatus("reconnecting");
      // A few failures in a row: the sandbox may have stopped, or the
      // session ended. Asking again shows which (or goes to the login page).
      if (st.failures >= 2) {
        void props.onLost();
        void whoAmI(true).catch(() => {});
      }
      const wait = Math.min(10_000, 500 * 2 ** Math.min(st.failures, 5));
      st.timer = setTimeout(connect, wait);
    };
  };

  useEffect(() => {
    const t = new Terminal({
      fontFamily: 'ui-monospace, "SF Mono", Menlo, Consolas, "DejaVu Sans Mono", "Liberation Mono", monospace',
      fontSize: 13,
      lineHeight: 1.15,
      cursorBlink: true,
      scrollback: 2000,
      macOptionIsMeta: true,
      macOptionClickForcesSelection: true,
      allowTransparency: false,
      theme: THEME,
    });
    const f = new FitAddon();
    t.loadAddon(f);
    t.open(el.current!);
    term.current = t;
    fit.current = f;
    const enc = new TextEncoder();
    const send = (b: Uint8Array<ArrayBuffer>) => {
      if (ws.current?.readyState === WebSocket.OPEN) ws.current.send(b);
    };
    t.onData((d) => send(enc.encode(d)));
    t.onBinary((d) => send(Uint8Array.from(d, (c) => c.charCodeAt(0))));
    t.onResize(({ cols, rows }) => {
      if (ws.current?.readyState === WebSocket.OPEN) ws.current.send(JSON.stringify({ resize: [cols, rows] }));
    });
    const ro = new ResizeObserver(() => {
      if (el.current?.offsetParent) {
        try {
          f.fit();
        } catch {}
      }
    });
    ro.observe(el.current!);
    try {
      f.fit();
    } catch {}
    connect();
    const ping = setInterval(() => {
      if (document.visibilityState === "visible" && ws.current?.readyState === WebSocket.OPEN) ws.current.send(JSON.stringify({ ping: 1 }));
    }, 60_000);
    return () => {
      const st = state.current;
      st.disposed = true;
      clearTimeout(st.timer);
      clearInterval(ping);
      ro.disconnect();
      const sock = ws.current;
      if (sock) {
        // A tab closed in the dashboard ends its tmux session too.
        if (st.killing && sock.readyState === WebSocket.OPEN) sock.send(JSON.stringify({ kill: 1 }));
        setTimeout(() => sock.close(), st.killing ? 300 : 0);
      }
      t.dispose();
    };
  }, []);

  // Back to running after a stop: a fresh shell.
  useEffect(() => {
    if (props.running && !ws.current && !state.current.ended && term.current) {
      state.current.failures = 0;
      connect();
    }
  }, [props.running]);

  useEffect(() => {
    if (!props.visible) return;
    requestAnimationFrame(() => {
      try {
        fit.current?.fit();
      } catch {}
      term.current?.focus();
    });
  }, [props.visible]);

  const restart = () => {
    state.current.ended = false;
    setExit(null);
    setStatus("connecting");
    connect();
  };

  // Marks the session to be killed as the component goes.
  useEffect(() => {
    const k = () => (state.current.killing = true);
    window.addEventListener(`cx-kill-${props.tab.id}`, k);
    return () => window.removeEventListener(`cx-kill-${props.tab.id}`, k);
  }, []);

  return (
    <div class="term" hidden={!props.visible}>
      <div class="term-status">
        {status === "reconnecting" ? <span class="led-text wait">reconnecting…</span> : null}
        {status === "connecting" ? <span class="muted">connecting…</span> : null}
        {status === "exited" ? (
          <span>
            The shell exited{exit !== null && exit !== 0 ? ` (exit ${exit})` : ""}.{" "}
            <button type="button" class="btn btn-quiet" onClick={restart}>
              Start a new shell
            </button>
            <button
              type="button"
              class="btn btn-quiet"
              onClick={() => {
                props.onClosed();
              }}
            >
              Close the tab
            </button>
          </span>
        ) : null}
      </div>
      <div class="term-screen" ref={el} />
    </div>
  );
}
