// Which control socket is a node's, and whether the node is online: pure
// functions, so they can be tested without the Workers runtime (node.ts uses
// them). Both rules come from the spikes (research/node-spikes.md, "Stale
// control socket"): after an edge reset the Node object can keep a dead socket
// listed as open next to the new one, or keep one that nobody answers on.
//
// - The node's socket is the newest open one by generation, never the first
//   that getWebSockets() returns.
// - It's online only while that socket's last sign of life (the agent's
//   "ping", answered by auto-response without waking the object, or any
//   message) is under 60 s old. The agent pings every 10 s.

export const ONLINE_WITHIN_MS = 60_000;

export type ControlSocket<W> = {
  ws: W;
  gen: number;
  open: boolean;
  // When the socket was accepted, its last message, and the runtime's
  // auto-response timestamp for "ping" (null before the first).
  acceptedAt: number;
  lastMessageAt: number | null;
  lastPingAt: number | null;
};

export function newest<W>(sockets: ControlSocket<W>[]): ControlSocket<W> | null {
  let best: ControlSocket<W> | null = null;
  for (const s of sockets) if (s.open && (best === null || s.gen > best.gen)) best = s;
  return best;
}

export const lastSign = (s: ControlSocket<unknown>) => Math.max(s.acceptedAt, s.lastMessageAt ?? 0, s.lastPingAt ?? 0);

export const fresh = (s: ControlSocket<unknown>, now: number) => now - lastSign(s) < ONLINE_WITHIN_MS;

// The socket to use and whether the node is online, and the sockets to close:
// every open one but the newest, and the newest too once it's gone quiet.
export function pick<W>(sockets: ControlSocket<W>[], now: number): { use: ControlSocket<W> | null; online: boolean; close: ControlSocket<W>[] } {
  const best = newest(sockets);
  const online = best !== null && fresh(best, now);
  const close = sockets.filter((s) => s.open && (s !== best || !online));
  return { use: online ? best : null, online, close };
}

// The Durable Object region for a node's sandboxes, from where its agent's
// connection reached Cloudflare (request.cf). Hints are honored by continent
// (M0 Q3), so North America and Europe are split by longitude. South America,
// Africa and the Middle East have no region of their own: no hint, and the
// object lands near the request that makes it.
export type LocationHint = "wnam" | "enam" | "weur" | "eeur" | "apac" | "oc";

export function locationHint(cf: { continent?: unknown; longitude?: unknown } | undefined): LocationHint | null {
  if (!cf) return null;
  const lon = Number(cf.longitude);
  const known = cf.longitude !== undefined && cf.longitude !== null && cf.longitude !== "" && Number.isFinite(lon);
  switch (cf.continent) {
    case "NA":
      // Dallas (-96.8) is in wnam, Chicago (-87.6) in enam (M0 Q3).
      return known && lon < -95 ? "wnam" : "enam";
    case "EU":
      return known && lon >= 15 ? "eeur" : "weur";
    case "AS":
      // The Middle East is in Asia to Cloudflare; it lies west of 60°E.
      return known && lon < 60 ? null : "apac";
    case "OC":
      return "oc";
    default:
      return null;
  }
}
