// The dashboard (apps/dashboard), served at / on the API's own host, so it
// needs no domain of its own. scripts/bundle-api.sh builds it into
// apps/dashboard/dist first, and its files go into worker.js with the rest.
// Each file is served under a name with its hash, cached for good; the page
// itself is never cached, so a deploy shows at the next load.
//
// Nothing from a sandbox is ever served here (see session.ts): the dashboard
// shows agent output, logs and file names as text, the desktop through its
// own noVNC, and previews only on the preview domain.
import { appCss, appJs, archivo, martianMono, vncJs } from "cloudex-dashboard-assets";
import { VERSION } from "./util";

const unbase64 = (s: string) => Uint8Array.from(atob(s), (c) => c.charCodeAt(0)).buffer;

const ICON = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" fill="#0b0a09"/><rect x="6" y="6" width="20" height="20" fill="none" stroke="#f38020" stroke-width="4"/><rect x="13" y="13" width="6" height="6" fill="#f38020"/></svg>`;

type Asset = { name: string; ext: string; type: string; body: string | ArrayBuffer; hashed: boolean };
const FILES: Asset[] = [
  { name: "app", ext: "js", type: "text/javascript; charset=utf-8", body: appJs, hashed: true },
  { name: "app", ext: "css", type: "text/css; charset=utf-8", body: appCss, hashed: true },
  { name: "vnc", ext: "js", type: "text/javascript; charset=utf-8", body: vncJs, hashed: true },
  // The stylesheet names these two as they are.
  { name: "archivo", ext: "woff2", type: "font/woff2", body: unbase64(archivo), hashed: false },
  { name: "martian-mono", ext: "woff2", type: "font/woff2", body: unbase64(martianMono), hashed: false },
  { name: "icon", ext: "svg", type: "image/svg+xml", body: ICON, hashed: false },
];

// Each file's served name: app.3f9c2a1b7d.js. Worked out once per isolate.
let names: Promise<Map<string, Asset>> | undefined;
function served() {
  names ??= Promise.all(
    FILES.map(async (f) => {
      if (!f.hashed) return [`${f.name}.${f.ext}`, f] as const;
      const bytes = typeof f.body === "string" ? new TextEncoder().encode(f.body) : f.body;
      const d = new Uint8Array(await crypto.subtle.digest("SHA-256", bytes));
      const hash = [...d.slice(0, 5)].map((b) => b.toString(16).padStart(2, "0")).join("");
      return [`${f.name}.${hash}.${f.ext}`, f] as const;
    }),
  ).then((pairs) => new Map(pairs));
  return names;
}

const servedName = (all: Map<string, Asset>, name: string, ext: string) =>
  [...all].find(([, f]) => f.name === name && f.ext === ext)![0];

// What every dashboard answer carries. No inline scripts anywhere; styles may
// be inline because xterm.js sets its theme with a style element.
function headers(req: Request, type: string, cache: string) {
  const host = new URL(req.url).host;
  const ws = new URL(req.url).protocol === "http:" ? `ws://${host}` : `wss://${host}`;
  return {
    "content-type": type,
    "cache-control": cache,
    "content-security-policy": [
      "default-src 'self'",
      "script-src 'self'",
      "style-src 'self' 'unsafe-inline'",
      "img-src 'self' data: blob:",
      "font-src 'self'",
      `connect-src 'self' ${ws}`,
      "frame-ancestors 'none'",
      "base-uri 'none'",
      "form-action 'none'",
      "object-src 'none'",
    ].join("; "),
    "x-content-type-options": "nosniff",
    "x-frame-options": "DENY",
    "referrer-policy": "no-referrer",
    "cross-origin-opener-policy": "same-origin",
    "cross-origin-resource-policy": "same-origin",
    "permissions-policy": "camera=(), microphone=(), geolocation=(), payment=(), usb=()",
  };
}

export async function serveDashboard(req: Request): Promise<Response> {
  const path = new URL(req.url).pathname;
  const all = await served();
  const head = req.method === "HEAD";
  const asset = /^\/assets\/([a-z0-9.-]+)$/.exec(path)?.[1];
  if (asset !== undefined) {
    const f = all.get(asset);
    if (f) return new Response(head ? null : f.body, { headers: headers(req, f.type, f.hashed ? "public, max-age=31536000, immutable" : "public, max-age=86400") });
    // A page from before a deploy asking for its old files gets the new ones,
    // uncached.
    const now = /^([a-z-]+)\.[0-9a-f]{10}\.(js|css)$/.exec(asset);
    const same = now && FILES.find((x) => x.hashed && x.name === now[1] && x.ext === now[2]);
    if (same) return new Response(head ? null : same.body, { headers: headers(req, same.type, "no-store") });
    return new Response("not found\n", { status: 404, headers: headers(req, "text/plain; charset=utf-8", "no-store") });
  }
  if (path === "/robots.txt") {
    return new Response("User-agent: *\nDisallow: /\n", { headers: headers(req, "text/plain; charset=utf-8", "public, max-age=86400") });
  }
  const page = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<meta name="cx-version" content="${VERSION}">
<meta name="cx-vnc" content="/assets/${servedName(all, "vnc", "js")}">
<title>Cloudex</title>
<link rel="icon" href="/assets/icon.svg" type="image/svg+xml">
<link rel="stylesheet" href="/assets/${servedName(all, "app", "css")}">
<script type="module" src="/assets/${servedName(all, "app", "js")}"></script>
</head>
<body>
<div id="app"></div>
<noscript>The Cloudex dashboard needs JavaScript.</noscript>
</body>
</html>
`;
  return new Response(head ? null : page, { headers: headers(req, "text/html; charset=utf-8", "no-store") });
}
