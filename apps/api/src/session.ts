// Dashboard sessions in the browser: the cookie that carries one, and the
// checks that keep another site from using it. A session is a token of kind
// session (tokens.ts); the cookie holds its secret, where scripts can't read
// it (HttpOnly), and only this host gets it (the __Host- prefix), never
// with a request another site starts (SameSite=Strict).
//
// Requests a cookie authenticates must also show they come from the dashboard
// itself: the Origin, when the browser sends one, is this host's, and the
// header X-Cloudex: 1, which a page on another site can't add without asking
// first (CORS), and this API never says yes. WebSockets can't carry headers, so
// for them the Origin must be there and be this host's. Downloads, which the
// dashboard opens as plain links, need Sec-Fetch-Site: same-origin instead.
// Bearer requests don't use any of this.
export const SESSION_COOKIE = "__Host-cx_session";

export function readSession(req: Request): string | null {
  const h = req.headers.get("cookie");
  if (!h) return null;
  for (const part of h.split(";")) {
    const i = part.indexOf("=");
    if (i > 0 && part.slice(0, i).trim() === SESSION_COOKIE) return part.slice(i + 1).trim() || null;
  }
  return null;
}

// The Set-Cookie value for a session's secret, or with null one that clears it.
export function sessionCookie(secret: string | null, maxAgeSec = 0) {
  const base = `${SESSION_COOKIE}=${secret ?? ""}; Path=/; Secure; HttpOnly; SameSite=Strict`;
  return `${base}; Max-Age=${secret ? maxAgeSec : 0}`;
}

const DOWNLOAD = /^\/v1\/sandboxes\/[^/]+\/files\/(content|archive)$/;

// Why this request, carrying a session cookie, might not come from the
// dashboard; null when it does. login: the login page trading a code for a
// session, which must have both the Origin and the header.
export function notFromDashboard(req: Request, login = false): string | null {
  const url = new URL(req.url);
  const origin = req.headers.get("origin");
  if (origin !== null && origin !== url.origin) return `it came from ${origin}, not from the dashboard`;
  const site = req.headers.get("sec-fetch-site");
  if (site !== null && site !== "same-origin" && site !== "none") return "it came from another site";
  const header = req.headers.get("x-cloudex") === "1";
  if (login) return header && origin === url.origin ? null : "only the dashboard's login page can sign in this way";
  if (header) return null;
  if (req.headers.get("upgrade")?.toLowerCase() === "websocket") {
    return origin === url.origin ? null : "a WebSocket signed in with a cookie needs the dashboard's Origin";
  }
  if ((req.method === "GET" || req.method === "HEAD") && DOWNLOAD.test(url.pathname) && site === "same-origin") return null;
  return "a request signed in with a cookie needs the header X-Cloudex: 1";
}

// What a new session is called in the token list: dashboard (Firefox on Linux).
export function sessionName(ua: string | undefined) {
  const s = ua ?? "";
  const browser = /Edg\//.test(s)
    ? "Edge"
    : /Firefox\//.test(s)
      ? "Firefox"
      : /Chrome\//.test(s)
        ? "Chrome"
        : /Safari\//.test(s)
          ? "Safari"
          : "a browser";
  const os = /iPhone|iPad/.test(s)
    ? "iOS"
    : /Android/.test(s)
      ? "Android"
      : /Mac OS X/.test(s)
        ? "macOS"
        : /Windows/.test(s)
          ? "Windows"
          : /Linux|X11/.test(s)
            ? "Linux"
            : "";
  return `dashboard (${browser}${os ? ` on ${os}` : ""})`;
}
