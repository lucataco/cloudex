// Placing sandboxes on nodes (v0.2.2 M5): which node may take a new sandbox,
// for whom, and with what limits, and how a sandbox on a node is reconciled
// with what its agent reports. Pure functions, so they can be tested without
// the Workers runtime; index.ts, sandbox.ts and node.ts use them.
import type { Isolation, Node, SandboxSize, SandboxState } from "@cloudex/schema";

// A size's CPUs and memory on Cloudflare, which a node sandbox gets by
// default: standard-2 is 1 CPU and 6 GiB.
export const SIZE_LIMITS: Record<SandboxSize, { cpus: number; memoryMb: number }> = {
  "standard-1": { cpus: 0.5, memoryMb: 4096 },
  "standard-2": { cpus: 1, memoryMb: 6144 },
  "standard-3": { cpus: 2, memoryMb: 8192 },
  "standard-4": { cpus: 4, memoryMb: 12288 },
};

// The agent's own floor for a machine's memory (go/internal/node/spec.go).
export const MIN_MEMORY_MB = 512;

// How a node's machines are kept apart (Isolation in @cloudex/schema): none
// for a node joined with --unisolated, else its runtime. Null before the
// agent has said.
export function nodeIsolation(n: { runtime: Node["runtime"]; isolation: Node["isolation"] }): Isolation | null {
  if (!n.isolation || !n.runtime) return null;
  if (n.isolation.unisolated) return "none";
  return n.runtime === "sysbox" ? "sysbox" : "docker";
}

// "2026-11-11 14:02 UTC", for messages.
export function seenText(ms: number | null | undefined): string {
  if (!ms) return "never seen";
  return `last seen ${new Date(ms).toISOString().slice(0, 16).replace("T", " ")} UTC`;
}

export const offlineMessage = (name: string, lastSeenAt: number | null | undefined) => `node ${name} is offline (${seenText(lastSeenAt)})`;

// While a sandbox's node is offline its alarm doesn't ask the node's object
// each minute: it tries again 10 minutes after it went down, then after 20,
// 40 and so on, at most every 6 hours (2026-11-12 review, W4). The node
// coming back says so itself (Sandbox.nodeReport), so nothing waits for this.
export const NODE_RETRY_FIRST_MS = 10 * 60_000;
export const NODE_RETRY_MAX_MS = 6 * 3600_000;
export function nodeRetryAt(lastTryAt: number, tries: number): number {
  return lastTryAt + Math.min(NODE_RETRY_FIRST_MS * 2 ** Math.min(tries, 16), NODE_RETRY_MAX_MS);
}

// The alarm of a running sandbox whose node is offline: whether it may ask
// the node's object now, and when it runs next.
export function offlineAlarm(down: { since: number; retryAt?: number } | null, now: number, everyMs: number): { ask: boolean; next: number } {
  if (!down) return { ask: true, next: now + everyMs };
  const retryAt = down.retryAt ?? nodeRetryAt(down.since, 0);
  return now < retryAt ? { ask: false, next: Math.max(retryAt, now + everyMs) } : { ask: true, next: now + everyMs };
}

export const removedMessage = (name: string) => `node ${name} was removed (cloudex node rm), so its sandboxes can't run any more; delete this one`;

export type Placer = { ownOnly: boolean };
export type PlaceRequest = { size: SandboxSize; cpus?: number; memoryMb?: number };
export type Placed = { cpus: number; memoryMb: number; isolation: Isolation };
export type Refusal = { status: number; error: string };

// Whether a new sandbox may go on node n (as of now: its state from its Node
// object), for a caller, and the limits its machine gets. Decision 4 of
// research/v0.2.2-plan.md: any token that may make sandboxes may place them
// on a node, except ownOnly ones (CI, which runs code from pull requests) on
// a node whose isolation is plain Docker or none.
export function placeOn(n: Node, who: Placer, req: PlaceRequest): Placed | Refusal {
  const name = n.name;
  switch (n.state) {
    case "pending":
      return { status: 409, error: `node ${name} hasn't joined yet: run the command cloudex node add printed on it, as root` };
    case "revoked":
      return { status: 404, error: `node ${name} was removed; add it again with cloudex node add ${name}` };
    case "offline":
      return { status: 503, error: offlineMessage(name, n.lastSeenAt) };
  }
  const iso = n.isolation;
  if (!iso || !n.runtime) return { status: 503, error: `node ${name} hasn't reported its host yet; try again in a minute` };
  if (!iso.unisolated) {
    // unchecked passes: the agent waits up to a minute for its self-test's
    // verdict (it runs one whenever it starts) and refuses a machine without
    // a passing one, saying why.
    if (iso.firewall === "fail") {
      const why = (iso.failed ?? []).slice(0, 3).join("; ");
      return {
        status: 503,
        error: `node ${name}'s isolation self-test fails${why ? ` (${why})` : ""}, so it runs no sandboxes; check its firewall (cloudex-node-firewall.service), then cloudex node info ${name}`,
      };
    }
  }
  if (n.image && n.image.state !== "present") {
    const how = n.image.state === "pulling" ? `is still pulling the sandbox image${n.image.progress !== undefined ? ` (${Math.round(n.image.progress * 100)}%)` : ""}; try again when cloudex node info ${name} says it's present` : `can't use the sandbox image (${n.image.state})`;
    return { status: 503, error: `node ${name} ${how}` };
  }
  const isolation = nodeIsolation(n)!;
  if (who.ownOnly && (isolation === "docker" || isolation === "none")) {
    return {
      status: 403,
      error:
        `an own-only token can't place sandboxes on node ${name}: it runs them under ${isolation === "none" ? "no isolation (--unisolated, testing only)" : "plain Docker"}, ` +
        "where code that escapes its container is root on the host. Use a node with Sysbox, or a token that isn't own-only",
    };
  }
  const def = SIZE_LIMITS[req.size];
  const hostCpus = n.cpus ?? null;
  const hostMb = n.memoryMb ?? null;
  if (req.cpus !== undefined && hostCpus !== null && req.cpus > hostCpus) {
    return { status: 400, error: `cpus: node ${name} has ${hostCpus} CPUs; ask for at most that` };
  }
  if (req.memoryMb !== undefined && hostMb !== null && req.memoryMb > hostMb) {
    return { status: 400, error: `memoryMb: node ${name} has ${hostMb} MiB of memory; ask for at most that` };
  }
  const cpus = req.cpus ?? (hostCpus !== null ? Math.min(def.cpus, hostCpus) : def.cpus);
  const memoryMb = req.memoryMb ?? (hostMb !== null ? Math.max(MIN_MEMORY_MB, Math.min(def.memoryMb, Math.floor(hostMb / 2))) : def.memoryMb);
  return { cpus, memoryMb, isolation };
}

export const refused = (p: Placed | Refusal): p is Refusal => "error" in p;

// ---------------------------------------------------------------- reconcile

// What a node's agent says of a sandbox's machine: running, stopped (its disk
// is there), or missing (the agent listed its machines and this one wasn't
// among them).
export type MachineReport = "running" | "stopped" | "missing";

// What a Sandbox object does about a report, given its own state, which is
// the truth about what should run (the agent never decides that):
// - mark-stopped: it was running, and its machine stopped on its own (a
//   reboot, an out-of-memory, a docker stop on the host);
// - mark-missing: it was running, and its machine is gone;
// - stop-machine: it's stopped, and its machine runs (started on the host by
//   hand, or a stop that was cut off): the machine is stopped again;
// - note-missing: it's stopped, and its machine is gone, so a resume starts
//   it over from the image: it says so;
// - none.
// starting and stopping are left to the start or stop under way, which runs
// ahead of any report (the lifecycle queue).
export type MachineAction = "none" | "mark-stopped" | "mark-missing" | "stop-machine" | "note-missing";

export function machineAction(state: SandboxState, machine: MachineReport): MachineAction {
  switch (state) {
    case "running":
      return machine === "running" ? "none" : machine === "stopped" ? "mark-stopped" : "mark-missing";
    case "stopped":
    case "error":
      return machine === "running" ? "stop-machine" : machine === "missing" ? "note-missing" : "none";
    default:
      return "none";
  }
}

// What a Node object does when its agent says hello with its machines:
// - deletes: the machines of sandboxes deleted while it was offline
//   (tombstones), each removed, and its tombstone dropped once that worked;
// - reports: each sandbox the index has on the node, with its machine's
//   state, or missing; when the agent couldn't list its machines
//   (machinesKnown false), only the ones it listed;
// - orphans: machines of no sandbox and no tombstone, which are left alone
//   and listed in cloudex node info.
export type ReconcilePlan = { deletes: string[]; reports: { id: string; machine: MachineReport }[]; orphans: string[] };

export function reconcilePlan(input: {
  machines: { id: string; state: "running" | "stopped" }[];
  machinesKnown: boolean;
  sandboxes: string[];
  tombstones: string[];
}): ReconcilePlan {
  const tomb = new Set(input.tombstones);
  const reported = new Map(input.machines.map((m) => [m.id, m.state]));
  const mine = new Set(input.sandboxes);
  const reports: ReconcilePlan["reports"] = [];
  for (const id of input.sandboxes) {
    if (tomb.has(id)) continue;
    const m = reported.get(id);
    if (m) reports.push({ id, machine: m });
    else if (input.machinesKnown) reports.push({ id, machine: "missing" });
  }
  const orphans = input.machines.filter((m) => !mine.has(m.id) && !tomb.has(m.id)).map((m) => m.id);
  return { deletes: [...tomb], reports, orphans };
}
