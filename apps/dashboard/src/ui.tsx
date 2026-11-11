// Pieces every page uses: formatting, polling, status lights, buttons that
// show their call is running, and the error line.
import type { Sandbox } from "@cloudex/schema";
import type { ComponentChildren } from "preact";
import { useCallback, useEffect, useRef, useState } from "preact/hooks";

export function ago(ms: number | null | undefined) {
  if (!ms) return "–";
  const s = Math.round((Date.now() - ms) / 1000);
  if (s < 0) return "in " + span(-s);
  if (s < 5) return "just now";
  return span(s) + " ago";
}

export function until(ms: number | null | undefined) {
  if (!ms) return "never";
  const s = Math.round((ms - Date.now()) / 1000);
  return s <= 0 ? "expired" : "in " + span(s);
}

export function span(s: number) {
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h ${Math.floor((s % 3600) / 60)}m`;
  return `${Math.floor(s / 86400)}d`;
}

export function bytes(n: number | null | undefined) {
  if (n === null || n === undefined) return "–";
  if (n < 1024) return `${n} B`;
  const u = ["KB", "MB", "GB", "TB"];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return `${v < 10 ? v.toFixed(1) : Math.round(v)} ${u[i]}`;
}

export const when = (ms: number | null | undefined) => (ms ? new Date(ms).toLocaleString() : "–");

// Loads with fn now and every `every` ms while the page is visible.
export function usePoll<T>(fn: () => Promise<T>, deps: unknown[], every = 0) {
  const [data, setData] = useState<T | undefined>(undefined);
  const [error, setError] = useState<string | null>(null);
  const fnRef = useRef(fn);
  fnRef.current = fn;
  const load = useCallback(async () => {
    try {
      setData(await fnRef.current());
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
  }, []);
  useEffect(() => {
    setData(undefined);
    void load();
    if (!every) return;
    const t = setInterval(() => {
      if (document.visibilityState === "visible") void load();
    }, every);
    const onVisible = () => document.visibilityState === "visible" && void load();
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      clearInterval(t);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, deps);
  return { data, error, reload: load };
}

const LED: Record<string, string> = {
  running: "run",
  starting: "wait",
  stopping: "wait",
  idle: "off",
  stopped: "off",
  exited: "off",
  error: "bad",
  killed: "bad",
  lost: "bad",
};

export function State(props: { state: string }) {
  return (
    <span class="state">
      <i class={`led led-${LED[props.state] ?? "off"}`} aria-hidden="true" />
      {props.state}
    </span>
  );
}

// A button whose action is a call: disabled while it runs, its error shown
// through onError.
export function Act(props: {
  run: () => Promise<unknown>;
  onError?: (e: string | null) => void;
  children: ComponentChildren;
  kind?: "primary" | "danger" | "quiet";
  confirm?: string;
  disabled?: boolean;
  title?: string;
  busy?: string;
}) {
  const [busy, setBusy] = useState(false);
  const click = async () => {
    if (props.confirm && !confirm(props.confirm)) return;
    setBusy(true);
    props.onError?.(null);
    try {
      await props.run();
    } catch (e) {
      const why = e instanceof Error ? e.message : String(e);
      if (props.onError) props.onError(why);
      else alert(why);
    } finally {
      setBusy(false);
    }
  };
  return (
    <button type="button" class={`btn ${props.kind ? `btn-${props.kind}` : ""}`} disabled={busy || props.disabled} onClick={click} title={props.title}>
      {busy ? (props.busy ?? props.children) : props.children}
    </button>
  );
}

export function Problem(props: { error: string | null | undefined }) {
  if (!props.error) return null;
  return (
    <p class="problem" role="alert">
      {props.error}
    </p>
  );
}

export function Copy(props: { text: string; label?: string }) {
  const [done, setDone] = useState(false);
  return (
    <button
      type="button"
      class="btn btn-quiet"
      onClick={async () => {
        await navigator.clipboard.writeText(props.text);
        setDone(true);
        setTimeout(() => setDone(false), 1200);
      }}
    >
      {done ? "copied" : (props.label ?? "copy")}
    </button>
  );
}

// A command to copy, in the monospace face.
export function Cmd(props: { text: string }) {
  return (
    <div class="cmd">
      <code>{props.text}</code>
      <Copy text={props.text} />
    </div>
  );
}

export function Loading(props: { what?: string }) {
  return <p class="muted">Loading{props.what ? ` ${props.what}` : ""}…</p>;
}

export const SIZES = ["standard-1", "standard-2", "standard-3", "standard-4"] as const;
export const SIZE_NOTE: Record<string, string> = {
  "standard-1": "1/2 vCPU, 4 GiB, 8 GB disk",
  "standard-2": "1 vCPU, 6 GiB, 12 GB disk",
  "standard-3": "2 vCPU, 8 GiB, 16 GB disk",
  "standard-4": "4 vCPU, 12 GiB, 20 GB disk",
};

export const errText = (e: unknown) => (e instanceof Error ? e.message : String(e));

// Where a sandbox runs (v0.2.2): Cloudflare, or a node by name, with whether
// it's offline or was removed. A node's name and everything about it come
// from outside the deployment's code, so all of it is shown as text only.
export function whereText(s: Sandbox): string {
  const n = s.placement?.provider === "node" ? s.placement.node : undefined;
  if (s.placement?.provider !== "node") return "Cloudflare";
  if (!n) return "a node";
  if (n.state === null || n.state === "revoked") return `${n.name} (removed)`;
  return n.online ? n.name : `${n.name} (offline)`;
}

export const onNode = (s: Sandbox) => s.placement?.provider === "node";

export const ISOLATION_TEXT: Record<string, string> = {
  vm: "VM (Cloudflare)",
  sysbox: "Sysbox",
  docker: "plain Docker: trusted code only",
  none: "none: testing only",
};

// What a node's machines run under, from its own report.
export function nodeIsolationText(n: { runtime: string | null; isolation: { firewall: string; unisolated: boolean } | null }): string {
  if (!n.isolation) return "–";
  if (n.isolation.unisolated) return ISOLATION_TEXT.none!;
  if (n.isolation.firewall === "fail") return "self-test failing: runs no sandboxes";
  if (n.isolation.firewall === "unchecked") return "self-test not run yet";
  return n.runtime === "sysbox" ? ISOLATION_TEXT.sysbox! : ISOLATION_TEXT.docker!;
}

export function limitsText(s: Sandbox): string {
  const p = s.placement;
  if (!p || p.cpus === undefined || p.memoryMb === undefined) return "–";
  return `${p.cpus} CPU${p.cpus === 1 ? "" : "s"}, ${(p.memoryMb / 1024).toFixed(1)} GiB`;
}
