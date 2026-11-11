// Nodes: a user's own Docker host that runs sandboxes for the deployment. An
// agent on the host (cloudex-node, go/internal/node) dials out to the Worker
// and keeps a control WebSocket open to the node's Durable Object.
//
// Two kinds of model live here:
// - the API's records (Node, CreateNode, NewNode), as clients see them;
// - the agent's messages, which both ends must agree on. The Go agent and
//   these models are checked against the same examples in fixtures/node
//   (test/node.test.ts here, go/internal/node/protocol_test.go there), so
//   neither side can rename or retype a field alone.
//
// Everything an agent sends is untrusted: sizes and lengths are bounded here,
// and the Worker stores only what these models let through.
import { z } from "zod";

// The control protocol's version. The Worker refuses an agent that speaks
// another, with a hint to upgrade it.
export const NODE_PROTOCOL = 1;

// Close codes on the control socket.
export const NodeClose = {
  replaced: 4000, // a newer connection of the same node took over
  revoked: 4003, // the node was removed: its key no longer works
  stale: 4008, // no heartbeat for too long; the agent reconnects
  badMessage: 4400, // a message that isn't valid JSON of the protocol, or too big
} as const;

// The most a control message may be, in bytes (UTF-16 code units on the Worker).
export const NODE_MESSAGE_MAX = 65_536;

export const NodeId = z.string().regex(/^nd-[a-z0-9]{8}$/, "a node id, nd-xxxxxxxx");

export const NodeState = z.enum(["pending", "online", "offline", "revoked"]);
export type NodeState = z.infer<typeof NodeState>;

export const NodeRuntime = z.enum(["docker", "sysbox"]);
export type NodeRuntime = z.infer<typeof NodeRuntime>;

// firewall: the result of the agent's self-test of the machine firewall
// (unchecked until it has run one). unisolated: joined with --unisolated, for
// testing only; machines there reach the host and its network.
export const NodeIsolation = z.object({
  firewall: z.enum(["pass", "fail", "unchecked"]),
  unisolated: z.boolean(),
});
export type NodeIsolation = z.infer<typeof NodeIsolation>;

export const AgentVersion = z.string().regex(/^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$/, "a version, such as v0.2.2");

// What the agent reports about its host. Unknown sizes are null.
export const NodeFacts = z.object({
  arch: z.enum(["amd64", "arm64"]),
  runtime: NodeRuntime,
  isolation: NodeIsolation,
  cpus: z.number().int().min(1).max(4096).nullable(),
  memoryMb: z.number().int().min(1).max(64 * 1024 * 1024).nullable(),
  diskFreeGb: z.number().min(0).max(1_000_000).nullable(),
});
export type NodeFacts = z.infer<typeof NodeFacts>;

// ---------------------------------------------------------------- the API's records

export const Node = z.object({
  id: z.string(), // nd-xxxxxxxx
  name: z.string(),
  // pending until the agent joins with its token; online while its control
  // socket is up and answering; revoked after cloudex node rm.
  state: NodeState,
  createdAt: z.number().int(),
  lastSeenAt: z.number().int().nullable(),
  version: z.string().nullable(), // the agent's
  protocol: z.number().int().nullable(),
  arch: z.string().nullable(),
  runtime: NodeRuntime.nullable(),
  isolation: NodeIsolation.nullable(),
  cpus: z.number().int().nullable(),
  memoryMb: z.number().int().nullable(),
  diskFreeGb: z.number().nullable(),
  // Where the agent's connection reached Cloudflare, and the Durable Object
  // region its sandboxes are made in (wnam, enam, weur, eeur, apac, oc), if any.
  colo: z.string().nullable(),
  continent: z.string().nullable(),
  locationHint: z.string().nullable(),
  machines: z.number().int(), // sandboxes on it
  revokedAt: z.number().int().nullable(),
  // GET /v1/nodes/{node} only, while it's online: a round trip to the agent, in ms.
  rttMs: z.number().int().optional(),
});
export type Node = z.infer<typeof Node>;

// As a sandbox's name (Name in index.ts); nd- marks node ids.
export const NodeName = z
  .string()
  .regex(/^[a-z][a-z0-9-]{0,30}[a-z0-9]$/, "2-32 lowercase letters, digits and dashes, starting with a letter and not ending with a dash")
  .refine((n) => !n.startsWith("nd-"), "names can't start with nd-, which marks node ids");

export const CreateNode = z.object({ name: NodeName });
export type CreateNode = z.input<typeof CreateNode>;

// The answer to adding a node: the token its agent joins with, once, before
// expiresAt, and the command that installs the agent with it.
export const NewNode = z.object({
  node: Node,
  joinToken: z.string(),
  expiresAt: z.number().int(),
  install: z.string(),
});
export type NewNode = z.infer<typeof NewNode>;

// ---------------------------------------------------------------- the agent's messages

// POST /v1/node/join, signed with the new key (proof that the agent holds it).
export const JOIN_TOKEN = /^cxj_[a-z2-7]{40}$/;
export const NodeJoin = z.object({
  token: z.string().max(100),
  publicKey: z.string().regex(/^[A-Za-z0-9+/]{43}=$/, "a raw Ed25519 public key in base64"),
  version: AgentVersion,
  facts: NodeFacts,
});
export type NodeJoin = z.infer<typeof NodeJoin>;

export const NodeJoined = z.object({ node: z.object({ id: z.string(), name: z.string() }) });
export type NodeJoined = z.infer<typeof NodeJoined>;

export const MachineState = z.enum(["running", "stopped"]);
export const NodeMachine = z.object({
  id: z.string().regex(/^cx-[a-z0-9]{6}$/, "a sandbox id"),
  state: MachineState,
});
export type NodeMachine = z.infer<typeof NodeMachine>;

const OpId = z.string().regex(/^op-[a-z0-9]{1,32}$/, "an op id");

// Agent to Worker, as JSON text frames. Besides these, the agent sends "ping"
// every 10 s, which the Node object answers "pong" without waking.
export const NodeHello = z.object({
  type: z.literal("hello"),
  protocol: z.number().int(),
  version: AgentVersion,
  facts: NodeFacts,
  machines: z.array(NodeMachine).max(1000),
});
export const NodeFactsMessage = z.object({ type: z.literal("facts"), facts: NodeFacts });
export const NodeStateMessage = z.object({ type: z.literal("state"), machine: NodeMachine });
// An op's answer: value when ok, else error.
export const NodeResult = z.object({
  type: z.literal("result"),
  id: OpId,
  ok: z.boolean(),
  value: z.record(z.string(), z.unknown()).optional(),
  error: z.string().max(2000).optional(),
});
export const NodeMessage = z.discriminatedUnion("type", [NodeHello, NodeFactsMessage, NodeStateMessage, NodeResult]);
export type NodeMessage = z.infer<typeof NodeMessage>;

// Worker to agent. The set of ops is fixed: none of them runs anything on the
// host. M2 has ping (an empty answer, for round trips) and facts (fresh facts).
export const NodeOpName = z.enum(["ping", "facts"]);
export type NodeOpName = z.infer<typeof NodeOpName>;
export const NodeOp = z.object({
  type: z.literal("op"),
  id: OpId,
  name: NodeOpName,
  args: z.record(z.string(), z.unknown()),
});
export const WorkerMessage = z.discriminatedUnion("type", [NodeOp]);
export type WorkerMessage = z.infer<typeof WorkerMessage>;
