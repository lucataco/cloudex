// What each API route needs from an access token (tokens.ts), matched by the
// middleware in index.ts before any route runs. No imports, so that a test can
// check it against the OpenAPI document (test/rules.test.ts).

// What a route needs. read < write < admin; "any" is any caller with a key.
export type Need = "any" | "read" | "write" | "admin";

// [method, path, need, notOwnOnly]. :ref is a sandbox, which an ownOnly token
// must have made; notOwnOnly routes are closed to ownOnly tokens altogether.
// Every other route is for admins: tokens, provider keys, device approvals.
export const RULES: [string, string, Need, boolean?][] = [
  ["GET", "/v1/me", "any"],
  // A dashboard sign-in with the caller's own role.
  ["POST", "/v1/auth/links", "any"],
  ["GET", "/v1/sandboxes", "read"],
  ["POST", "/v1/sandboxes", "write"],
  ["GET", "/v1/sandboxes/:ref", "read"],
  ["PATCH", "/v1/sandboxes/:ref", "write"],
  ["DELETE", "/v1/sandboxes/:ref", "write"],
  ["POST", "/v1/sandboxes/:ref/stop", "write"],
  ["POST", "/v1/sandboxes/:ref/resume", "write"],
  ["POST", "/v1/sandboxes/:ref/exec", "write"],
  ["POST", "/v1/sandboxes/:ref/commands", "write"],
  ["GET", "/v1/sandboxes/:ref/commands", "read"],
  ["GET", "/v1/sandboxes/:ref/commands/:id", "read"],
  ["GET", "/v1/sandboxes/:ref/commands/:id/output", "read"],
  ["POST", "/v1/sandboxes/:ref/commands/:id/kill", "write"],
  ["DELETE", "/v1/sandboxes/:ref/commands/:id", "write"],
  ["GET", "/v1/sandboxes/:ref/files", "read"],
  ["DELETE", "/v1/sandboxes/:ref/files", "write"],
  ["GET", "/v1/sandboxes/:ref/files/content", "read"],
  ["PUT", "/v1/sandboxes/:ref/files/content", "write"],
  ["GET", "/v1/sandboxes/:ref/files/archive", "read"],
  ["PUT", "/v1/sandboxes/:ref/files/archive", "write"],
  ["POST", "/v1/sandboxes/:ref/fork", "write"],
  ["POST", "/v1/sandboxes/:ref/saves", "write", true],
  ["POST", "/v1/sandboxes/:ref/desktop", "write"],
  ["DELETE", "/v1/sandboxes/:ref/desktop", "write"],
  ["POST", "/v1/sandboxes/:ref/agents", "write"],
  ["GET", "/v1/sandboxes/:ref/agents", "read"],
  ["GET", "/v1/sandboxes/:ref/agents/:id", "read"],
  ["GET", "/v1/sandboxes/:ref/agents/:id/events", "read"],
  ["POST", "/v1/sandboxes/:ref/agents/:id/messages", "write"],
  ["POST", "/v1/sandboxes/:ref/agents/:id/interrupt", "write"],
  ["GET", "/v1/sandboxes/:ref/tcp/:port", "write"],
  ["GET", "/v1/sandboxes/:ref/terminal", "write"],
  ["GET", "/v1/sandboxes/:ref/desktop/vnc", "write"],
  // Links let anyone holding them into the sandbox's servers.
  ["POST", "/v1/sandboxes/:ref/hosts", "write"],
  ["GET", "/v1/sandboxes/:ref/hosts", "write"],
  ["DELETE", "/v1/sandboxes/:ref/hosts", "write"],
  ["DELETE", "/v1/sandboxes/:ref/hosts/:port", "write"],
  // Names and paths only; values are never returned.
  ["GET", "/v1/sandboxes/:ref/settings", "read"],
  ["PATCH", "/v1/sandboxes/:ref/settings", "write"],
  ["GET", "/v1/saves", "read"],
  ["GET", "/v1/saves/:name", "read"],
  ["DELETE", "/v1/saves/:name", "write", true],
  ["POST", "/v1/saves/:name/sandboxes", "write"],
];

const COMPILED = RULES.map(([method, path, need, notOwnOnly]) => ({
  method,
  re: new RegExp(`^${path.replace(/:ref/, "(?<ref>[^/]+)").replace(/:[a-z]+/g, "[^/]+")}$`),
  need,
  notOwnOnly: notOwnOnly ?? false,
}));

export function ruleFor(method: string, path: string) {
  for (const r of COMPILED) {
    // HEAD is a GET without the body.
    if (r.method !== (method === "HEAD" ? "GET" : method)) continue;
    const m = r.re.exec(path);
    if (m) return { need: r.need, notOwnOnly: r.notOwnOnly, ref: m.groups?.ref ?? null };
  }
  return { need: "admin" as Need, notOwnOnly: false, ref: null };
}

