// Calls to this deployment's /v1 API. The browser sends the session cookie;
// every call adds X-Cloudex: 1, which the API asks of cookie requests so that
// another site can't make them (apps/api/src/session.ts).
import type { Me } from "@cloudex/schema";

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
  }
}

// The login page, then back here.
export function toLogin() {
  const here = location.pathname + location.search;
  location.assign(here === "/" ? "/login" : `/login?next=${encodeURIComponent(here)}`);
}

export async function api<T = unknown>(method: string, path: string, body?: unknown, opts: { login?: boolean } = {}): Promise<T> {
  const headers: Record<string, string> = { "x-cloudex": "1" };
  if (body !== undefined) headers["content-type"] = "application/json";
  const res = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: "same-origin",
    cache: "no-store",
  });
  const text = await res.text();
  let data: unknown = null;
  try {
    data = text ? JSON.parse(text) : null;
  } catch {
    data = null;
  }
  if (!res.ok) {
    if (res.status === 401 && opts.login !== false) toLogin();
    const why = (data as { error?: string } | null)?.error ?? (text.trim() || res.statusText);
    throw new ApiError(res.status, why);
  }
  return data as T;
}

export const get = <T>(path: string) => api<T>("GET", path);
export const post = <T>(path: string, body?: unknown) => api<T>("POST", path, body ?? {});
export const patch = <T>(path: string, body: unknown) => api<T>("PATCH", path, body);
export const put = <T>(path: string, body: unknown) => api<T>("PUT", path, body);
export const del = <T>(path: string) => api<T>("DELETE", path);

export const sb = (ref: string) => `/v1/sandboxes/${encodeURIComponent(ref)}`;

// The close code of a WebSocket whose token or session ended while it was
// open (CREDENTIAL_ENDED in apps/api/src/util.ts): sign in again rather than
// reconnecting.
export const SIGNED_OUT = 4401;

// A WebSocket URL on this host. The cookie goes with it, and the API checks
// its Origin.
export const wsUrl = (path: string) => `${location.protocol === "http:" ? "ws" : "wss"}://${location.host}${path}`;

// Uploads with progress, which fetch can't report.
export function upload(path: string, file: Blob, progress: (sent: number, total: number) => void): Promise<unknown> {
  return new Promise((resolve, reject) => {
    const x = new XMLHttpRequest();
    x.open("PUT", path);
    x.setRequestHeader("x-cloudex", "1");
    x.upload.onprogress = (e) => progress(e.loaded, e.total);
    x.onload = () => {
      let data: { error?: string } | null = null;
      try {
        data = JSON.parse(x.responseText);
      } catch {}
      if (x.status >= 200 && x.status < 300) resolve(data);
      else {
        if (x.status === 401) toLogin();
        reject(new ApiError(x.status, data?.error ?? (x.responseText.trim() || `upload failed (${x.status})`)));
      }
    };
    x.onerror = () => reject(new ApiError(0, "the upload broke off"));
    x.send(file);
  });
}

let meCache: Promise<Me> | undefined;
export const whoAmI = (fresh = false) => {
  if (fresh || !meCache) meCache = get<Me>("/v1/me").catch((e) => ((meCache = undefined), Promise.reject(e)));
  return meCache;
};

export const version = () => document.querySelector<HTMLMetaElement>('meta[name="cx-version"]')?.content ?? "";
