// A small router over the History API: the Worker answers every dashboard
// path with the same page.
import type { ComponentChildren, JSX } from "preact";
import { useEffect, useState } from "preact/hooks";

const listeners = new Set<() => void>();
const changed = () => listeners.forEach((f) => f());
addEventListener("popstate", changed);

export function navigate(to: string, replace = false) {
  if (to === location.pathname + location.search) return;
  if (replace) history.replaceState(null, "", to);
  else history.pushState(null, "", to);
  scrollTo(0, 0);
  changed();
}

export function useLocation() {
  const [, set] = useState(0);
  useEffect(() => {
    const f = () => set((n) => n + 1);
    listeners.add(f);
    return () => void listeners.delete(f);
  }, []);
  return { path: location.pathname, query: new URLSearchParams(location.search) };
}

// Matches /s/:ref/agents/:id style patterns; the values come decoded.
export function match(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split("/");
  const s = path.replace(/\/+$/, "").split("/");
  if (p.length !== s.length && !(pattern.endsWith("/*") && s.length >= p.length - 1)) return null;
  const out: Record<string, string> = {};
  for (let i = 0; i < p.length; i++) {
    const want = p[i]!;
    if (want === "*") return out;
    const got = s[i] ?? "";
    if (want.startsWith(":")) {
      if (!got) return null;
      out[want.slice(1)] = decodeURIComponent(got);
    } else if (want !== got) return null;
  }
  return out;
}

export function Link(props: { href: string; class?: string; children: ComponentChildren; title?: string }) {
  const onClick = (e: JSX.TargetedMouseEvent<HTMLAnchorElement>) => {
    if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(props.href);
  };
  return (
    <a href={props.href} class={props.class} title={props.title} onClick={onClick}>
      {props.children}
    </a>
  );
}
