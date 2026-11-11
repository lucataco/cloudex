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

// Text a node writes that people read (its self-test's findings, its
// agent's errors): control characters other than newline and tab, and
// bidirectional overrides, become "?", so that it can't move a terminal's
// cursor, set its title or reorder what's shown (2026-11-12 review, W3). The
// CLI cleans what it prints again.
// eslint-disable-next-line no-control-regex
const CONTROLS = /[\u0000-\u0008\u000b-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/g;
export const stripControls = (s: string) => s.replace(CONTROLS, "?");
const NodeText = (max: number) => z.string().max(max).overwrite(stripControls);

export const NodeId = z.string().regex(/^nd-[a-z0-9]{8}$/, "a node id, nd-xxxxxxxx");

export const NodeState = z.enum(["pending", "online", "offline", "revoked"]);
export type NodeState = z.infer<typeof NodeState>;

export const NodeRuntime = z.enum(["docker", "sysbox"]);
export type NodeRuntime = z.infer<typeof NodeRuntime>;

// firewall: the result of the agent's self-test of the machine firewall
// (unchecked until it has run one). unisolated: joined with --unisolated, for
// testing only; machines there reach the host and its network. checkedAt and
// failed (v0.2.2 M4): when the self-test last ran, and the checks that failed.
export const NodeIsolation = z.object({
  firewall: z.enum(["pass", "fail", "unchecked"]),
  unisolated: z.boolean(),
  checkedAt: z.number().int().min(0).optional(),
  failed: z.array(NodeText(200)).max(16).optional(),
});
export type NodeIsolation = z.infer<typeof NodeIsolation>;

export const AgentVersion = z.string().regex(/^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$/, "a version, such as v0.2.2");

// The machine image (v0.2.2 M4) and whether the node has it: present,
// pulling (progress from 0 to 1), missing, or refused (not on the node's
// list of allowed images).
export const NodeImage = z.object({
  ref: NodeText(400),
  state: z.enum(["present", "pulling", "missing", "refused"]),
  progress: z.number().min(0).max(1).optional(),
});
export type NodeImage = z.infer<typeof NodeImage>;

// What the agent reports about its host. Unknown sizes are null.
export const NodeFacts = z.object({
  arch: z.enum(["amd64", "arm64"]),
  runtime: NodeRuntime,
  isolation: NodeIsolation,
  cpus: z.number().int().min(1).max(4096).nullable(),
  memoryMb: z.number().int().min(1).max(64 * 1024 * 1024).nullable(),
  diskFreeGb: z.number().min(0).max(1_000_000).nullable(),
  image: NodeImage.optional(),
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
  // GET /v1/nodes/{node} only: the machine image, as the agent last reported it.
  image: NodeImage.optional(),
  // GET /v1/nodes/{node} only (v0.2.2 M5): machines the agent reported that
  // belong to no sandbox (left alone, and listed here), and sandboxes deleted
  // while the node was offline whose machines it removes when it's back.
  orphans: z.array(z.string()).optional(),
  pendingDeletes: z.array(z.string()).optional(),
  // GET /v1/nodes/{node} only: the agent's clock less the deployment's at its
  // last connect, in seconds (more than 60 either way is refused).
  clockSkewSec: z.number().int().optional(),
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

const TicketRequestId = z.string().regex(/^tk-[a-z0-9]{1,32}$/, "a ticket request id");
const SandboxId = z.string().regex(/^cx-[a-z0-9]{6}$/, "a sandbox id");

// Agent to Worker, as JSON text frames. Besides these, the agent sends "ping"
// every 10 s, which the Node object answers "pong" without waking.
// machinesError (v0.2.2 M5): the agent couldn't list its machines (Docker
// didn't answer), so machines is empty for that reason, not because it has none.
export const NodeHello = z.object({
  type: z.literal("hello"),
  protocol: z.number().int(),
  version: AgentVersion,
  facts: NodeFacts,
  machines: z.array(NodeMachine).max(1000),
  machinesError: NodeText(500).optional(),
});
export const NodeFactsMessage = z.object({ type: z.literal("facts"), facts: NodeFacts });
export const NodeStateMessage = z.object({ type: z.literal("state"), machine: NodeMachine });
// An op's answer: value when ok, else error.
export const NodeResult = z.object({
  type: z.literal("result"),
  id: OpId,
  ok: z.boolean(),
  value: z.record(z.string(), z.unknown()).optional(),
  error: NodeText(2000).optional(),
});
// Stream tickets (M3): the agent asks for count of them for one sandbox, to
// open stream sockets to it (StreamOpen below). The Node object answers with
// NodeTickets.
export const STREAM_TICKETS_MAX = 8;
export const NodeTicketRequest = z.object({
  type: z.literal("tickets"),
  id: TicketRequestId,
  sandbox: SandboxId,
  count: z.number().int().min(1).max(STREAM_TICKETS_MAX),
});
export const NodeMessage = z.discriminatedUnion("type", [NodeHello, NodeFactsMessage, NodeStateMessage, NodeResult, NodeTicketRequest]);
export type NodeMessage = z.infer<typeof NodeMessage>;

// Worker to agent. The set of ops is fixed: none of them runs anything on the
// host. M2 has ping (an empty answer, for round trips) and facts (fresh facts).
// M3 adds streams {sandbox, count}: open that many more spare stream sockets.
// M4 adds the Docker runtime (go/internal/node/runtime.go), each refused while
// the node's isolation self-test doesn't pass:
// - create {sandbox, image?, cpus?, memoryMb?, env?, hold?}: make the machine
//   and its network, not started; answers {address, container, state}, and
//   created: true when it made it (M5), not when it was there already;
// - start {sandbox, hold?}: start it and wait for its entrypoint (or, with
//   hold, for it to hold for a restore, M6); answers the same, and ca: the
//   node CA's certificate (M6), once the welcome has named the providers;
// - stop {sandbox}: stop it, keeping its disk;
// - delete {sandbox}: remove the machine, its volumes and its network;
// - selftest: run the isolation self-test now; answers {pass, failed, checks, tookMs}.
export const NodeOpName = z.enum(["ping", "facts", "streams", "create", "start", "stop", "delete", "selftest"]);
export type NodeOpName = z.infer<typeof NodeOpName>;
export const NodeOp = z.object({
  type: z.literal("op"),
  id: OpId,
  name: NodeOpName,
  args: z.record(z.string(), z.unknown()),
});
// The answer to NodeTicketRequest: tickets for stream sockets (see StreamOpen).
// An empty list, with error, when the Node object won't issue them.
export const NodeTickets = z.object({
  type: z.literal("tickets"),
  id: TicketRequestId,
  tickets: z.array(z.string().max(512)).max(STREAM_TICKETS_MAX),
  error: z.string().max(500).optional(),
});
// Sent after the agent's hello (v0.2.2 M4): the image machines run, which the
// agent pulls and checks against its own list of allowed images; and (M6) the
// hosts of the AI providers whose HTTPS calls the deployment intercepts,
// which the agent's node CA is name-constrained to.
export const ProviderHost = z.string().regex(/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$/, "a host name").max(253);
export const NodeWelcome = z.object({
  type: z.literal("welcome"),
  image: z.string().max(400),
  providers: z.array(ProviderHost).max(32).optional(),
});

// The route a node's agent passes its machines' requests to cloudex.internal
// and to the providers on (v0.2.2 M6): EGRESS_ROUTE?target=<url>, signed. The
// agent refuses a body over EGRESS_BODY_MAX itself, since the edge would
// (M0 Q1), and sends /activity on at most every EGRESS_ACTIVITY_SEC.
export const EGRESS_ROUTE = "/v1/node/egress";
export const EGRESS_BODY_MAX = 100_000_000;
export const EGRESS_ACTIVITY_SEC = 30;
export const WorkerMessage = z.discriminatedUnion("type", [NodeOp, NodeTickets, NodeWelcome]);
export type WorkerMessage = z.infer<typeof WorkerMessage>;

// ---------------------------------------------------------------- streams (M3)
//
// A stream is a WebSocket the agent opens to GET /v1/node/streams/:sandbox/:stream
// with the header x-cx-ticket: a ticket the Node object issued over the
// control socket, good for that node, sandbox and stream id once, for 60 s.
// The Worker checks it without D1, and the sandbox's object accepts it only if
// its record names that node and it hasn't seen the stream id before. It is
// then parked as a spare until the object needs one, and opens it with one
// StreamOpen text frame.
//
// Afterwards:
// - Binary frames start with a channel byte: 0 is stdin, a request body or a
//   message to the machine; 1 is stdout, a response body or a message from
//   the machine; 2 is stderr. A preview's WebSocket uses 3 (to the machine)
//   and 4 (from it) for text messages.
// - Text frames are JSON control, StreamToAgent and StreamFromAgent, one key
//   each, or "cx-hb", which the agent sends every 15 s and the object answers
//   "cx-hb-ok" without waking.
// - After {"ok": true} to a tcp open with splice, the stream carries the
//   connection's bytes as they are, in binary frames with no channel byte
//   (ssh, whose own windows bound it, so the object can hibernate).
// - Every other channel has a window of STREAM_WINDOW bytes: a sender sends no
//   more than the receiver has granted with credit, and the window starts full.
export const STREAM_WINDOW = 4 << 20;
export const StreamChannel = { stdin: 0, stdout: 1, stderr: 2, textIn: 3, textOut: 4 } as const;
// Close codes on a stream socket, besides 1000.
export const StreamClose = {
  lost: 4404, // the object holding the stream restarted, or its other half went
  badTicket: 4401,
  badMessage: 4400,
  heartbeat: 4408, // no heartbeat for 45 s
} as const;

const Pty = z.object({ cols: z.number().int().min(1).max(1000), rows: z.number().int().min(1).max(1000) });
const HeaderList = z.array(z.tuple([z.string().max(256), z.string().max(16384)])).max(200);

export const StreamExec = z.object({
  argv: z.array(z.string().max(1 << 20)).min(1).max(4096),
  cwd: z.string().max(4096).optional(),
  env: z.record(z.string().max(256), z.string().max(1 << 16)).optional(),
  user: z.string().regex(/^[0-9a-z_-]{1,32}(:[0-9a-z_-]{1,32})?$/).optional(),
  pty: Pty.optional(),
  stdin: z.boolean(), // whether stdin comes on channel 0 (else it's closed at once)
  stderr: z.boolean(), // whether stderr is wanted on channel 2
});
export const StreamTcp = z.object({ port: z.number().int().min(1).max(65535), splice: z.boolean() });
export const StreamHttp = z.object({
  port: z.number().int().min(1).max(65535),
  method: z.string().regex(/^[A-Z]{1,16}$/),
  path: z.string().max(16384),
  headers: HeaderList,
  body: z.boolean(), // whether a body follows on channel 0
});
export const StreamOpen = z.union([
  z.object({ exec: StreamExec }).strict(),
  z.object({ tcp: StreamTcp }).strict(),
  z.object({ http: StreamHttp }).strict(),
]);
export type StreamOpen = z.infer<typeof StreamOpen>;

const Credit = z.object({ ch: z.number().int().min(0).max(4), n: z.number().int().min(1).max(1 << 30) });
const WsClose = z.object({ code: z.number().int().min(1000).max(4999), reason: NodeText(123) });

// The object to the agent, after the open.
export const StreamToAgent = z.union([
  StreamOpen,
  z.object({ resize: Pty }).strict(),
  z.object({ signal: z.number().int().min(1).max(64) }).strict(),
  z.object({ eof: z.literal(0) }).strict(), // no more stdin or body
  z.object({ credit: Credit }).strict(),
  z.object({ close: WsClose }).strict(),
]);
export type StreamToAgent = z.infer<typeof StreamToAgent>;

// The agent to the object.
export const StreamFromAgent = z.union([
  z.object({ ok: z.literal(true) }).strict(), // started, or connected
  z.object({ refused: NodeText(64) }).strict(), // nothing listens: the address tried
  z.object({ error: NodeText(2000) }).strict(), // couldn't start
  z.object({ status: z.object({ code: z.number().int().min(100).max(599), headers: HeaderList }) }).strict(),
  z.object({ eof: z.union([z.literal(1), z.literal(2)]) }).strict(),
  z.object({ exit: z.number().int().min(-1).max(255) }).strict(),
  z.object({ credit: Credit }).strict(),
  z.object({ close: WsClose }).strict(),
]);
export type StreamFromAgent = z.infer<typeof StreamFromAgent>;
