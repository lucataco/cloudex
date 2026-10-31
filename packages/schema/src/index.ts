// Models shared by the API Worker, the CLI and the SDKs. Keep this the single
// source of truth: the OpenAPI spec, and from it the SDKs, are generated here.
import { z } from "zod";

// Container instance types on Cloudflare. standard-4 is 4 vCPU, 12 GiB and 20 GB of disk.
// lite (256 MiB, 2 GB of disk) is left out: the sandbox image fails to boot on it.
export const SandboxSize = z.enum(["standard-1", "standard-2", "standard-3", "standard-4"]);
export type SandboxSize = z.infer<typeof SandboxSize>;

export const SandboxState = z.enum(["starting", "running", "stopping", "stopped", "error"]);
export type SandboxState = z.infer<typeof SandboxState>;

export const Sandbox = z.object({
  id: z.string(),
  name: z.string().nullable(),
  size: SandboxSize,
  state: SandboxState,
  idleMinutes: z.number().int(),
  createdAt: z.number().int(),
  updatedAt: z.number().int(),
  startedAt: z.number().int().nullable(),
  stoppedAt: z.number().int().nullable(),
  // When the disk was last saved, and the size of that save. Saves after a
  // resume hold only what changed since the previous one.
  savedAt: z.number().int().nullable(),
  savedBytes: z.number().int().nullable(),
  // The newest backup of /home (and the SSH host keys) in the account's R2
  // bucket. Unlike a save, it doesn't expire, and it restores onto a newer image.
  backupAt: z.number().int().nullable().optional(),
  backupBytes: z.number().int().nullable().optional(),
  backupError: z.string().nullable().optional(),
  // Something the user should know about the last start, such as a restore from backup.
  notice: z.string().nullable().optional(),
  // The sandbox this one is a fork of.
  forkedFrom: z.string().nullable().optional(),
  // Providers whose keys this run uses without holding them (see ProviderKey).
  providerKeys: z.array(z.string()).optional(),
  error: z.string().nullable(),
});
export type Sandbox = z.infer<typeof Sandbox>;

export const Name = z
  .string()
  .regex(/^[a-z][a-z0-9-]{0,30}[a-z0-9]$/, "lowercase letters, digits and dashes; 2-32 characters")
  .refine((n) => !n.startsWith("cx-"), "names can't start with cx-, which marks sandbox ids");

// Settings: environment variables and files the sandbox gets at every start,
// and a repo cloned when it's created. Variables reach every shell, ssh
// session, exec and agent; files go in the home directory, readable only by the
// cloudex user. The deployment keeps them encrypted, and never returns values
// or contents.
const RESERVED_ENV = new Set(["HOME", "USER", "LOGNAME", "SHELL", "PATH", "PWD"]);
export const EnvName = z
  .string()
  .regex(/^[A-Za-z_][A-Za-z0-9_]{0,127}$/, "letters, digits and underscores, not starting with a digit")
  .refine((n) => !RESERVED_ENV.has(n) && !n.startsWith("CLOUDEX_"), "reserved: set it in ~/.profile instead");
const EnvValue = z.string().max(32_768).refine((v) => !v.includes("\0"), "no NUL characters");

// In the home directory: ~/.npmrc, .npmrc and /home/cloudex/.npmrc are the same file.
export const FilePath = z
  .string()
  .min(1)
  .max(1024)
  .refine((p) => !p.includes("\0") && !/(^|\/)\.\.(\/|$)/.test(p), "no .. or NUL")
  .refine((p) => !p.startsWith("/") || p.startsWith("/home/cloudex/"), "a path in the home directory, such as ~/.npmrc");
const FileContent = z.string().max(262_144);

const Branch = z.string().regex(/^[\w./-]{1,200}$/, "a branch or tag name");
export const Repo = z.object({
  // HTTPS only. For a private GitHub repo, set GH_TOKEN or GITHUB_TOKEN in env.
  url: z.string().regex(/^https:\/\/[^\s#]+$/, "an https:// URL"),
  // Where to clone it, in the home directory; default the repo's name.
  dir: FilePath.optional(),
  branch: Branch.optional(),
});
export type Repo = z.input<typeof Repo>;

const maxEntries = (n: number) => (r: Record<string, unknown>) => Object.keys(r).length <= n;

export const CreateSandbox = z.object({
  name: Name.optional(),
  size: SandboxSize.default("standard-2"),
  // Stop (and save) after this many minutes without SSH, exec, preview or agent
  // activity. 0 keeps it running until the platform's 6-hour limit.
  idleMinutes: z.number().int().min(0).max(360).default(30),
  env: z.record(EnvName, EnvValue).refine(maxEntries(100), "at most 100 variables").optional(),
  files: z.record(FilePath, FileContent).refine(maxEntries(20), "at most 20 files").optional(),
  // An https:// URL, with #branch to pick one, or { url, dir, branch }. Cloned
  // once, when the sandbox is created.
  repo: z.union([z.string().regex(/^https:\/\/[^\s#]+(#[\w./-]{1,200})?$/, "an https:// URL, with #branch if you like"), Repo]).optional(),
});
export type CreateSandbox = z.input<typeof CreateSandbox>;

// A change to a sandbox's settings: null removes a variable or file. Applied at
// once to a running sandbox, and at every start.
export const UpdateSettings = z.object({
  env: z.record(EnvName, EnvValue.nullable()).optional(),
  files: z.record(FilePath, FileContent.nullable()).optional(),
});
export type UpdateSettings = z.input<typeof UpdateSettings>;

// What a sandbox has set: names and paths only.
export const SandboxSettings = z.object({
  env: z.array(z.string()),
  files: z.array(z.string()), // as ~/path
  repo: z.object({ url: z.string(), dir: z.string(), branch: z.string().nullable() }).nullable(),
});
export type SandboxSettings = z.infer<typeof SandboxSettings>;

// A fork is a new sandbox starting from a copy of another's disk: a fresh save
// of a running sandbox, which keeps running, or the last save of a stopped one.
// With count above 1 and a name, the forks are named <name>-1, <name>-2 and so
// on. Size and idle minutes default to the original's.
export const ForkRequest = z.object({
  name: Name.optional(),
  count: z.number().int().min(1).max(10).default(1),
  size: SandboxSize.optional(),
  idleMinutes: z.number().int().min(0).max(360).optional(),
});
export type ForkRequest = z.input<typeof ForkRequest>;

// fromBackup starts the image fresh and restores the newest backup, instead of
// resuming from the save. Resume does this by itself when the save has expired.
// size starts it on another size from now on; a smaller disk must still hold it.
export const ResumeRequest = z.object({ fromBackup: z.boolean().default(false), size: SandboxSize.optional() });
export type ResumeRequest = z.input<typeof ResumeRequest>;

// A change to the sandbox itself: its name, or how long it may sit idle before
// it stops. A new idle time applies at once, counting from now.
export const UpdateSandbox = z
  .object({
    name: Name.optional(),
    idleMinutes: z.number().int().min(0).max(360).optional(),
  })
  .refine((u) => u.name !== undefined || u.idleMinutes !== undefined, "give a name or idleMinutes");
export type UpdateSandbox = z.input<typeof UpdateSandbox>;

export const ExecRequest = z.object({
  cmd: z.string().min(1),
  root: z.boolean().default(false),
  cwd: z.string().optional(),
  timeoutSec: z.number().int().min(1).max(3600).default(600),
});
export type ExecRequest = z.input<typeof ExecRequest>;

export const ExecResult = z.object({
  exitCode: z.number().int(),
  stdout: z.string(),
  stderr: z.string(),
  timedOut: z.boolean(),
  ms: z.number().int(),
});
export type ExecResult = z.infer<typeof ExecResult>;

// AI providers whose keys the deployment can hold for sandboxes. A sandbox
// started while a provider has a key gets a placeholder, and the real key is
// added to its requests to that provider on the way out, so it never enters the
// sandbox, its saves, backups or forks.
export const Provider = z.enum(["anthropic", "openai"]);
export type Provider = z.infer<typeof Provider>;

export const SetProviderKey = z.object({
  key: z.string().min(8).max(1000).regex(/^\S+$/, "a key, without spaces"),
});
export type SetProviderKey = z.input<typeof SetProviderKey>;

// A stored key, which the API never returns: hint is its last 4 characters.
export const ProviderKey = z.object({
  provider: Provider,
  hint: z.string(),
  updatedAt: z.number().int(),
});
export type ProviderKey = z.infer<typeof ProviderKey>;

// Agents: Claude Code or Codex working on a task in a sandbox, without a
// terminal. Each message is one turn of the agent's own conversation; a message
// sent while a turn runs waits for it, or with interrupt, cuts it short. The
// conversation lives on the sandbox's disk, so saves, resumes and forks keep it.
export const AgentKind = z.enum(["claude", "codex"]);
export type AgentKind = z.infer<typeof AgentKind>;

// Up to 1 MB: prompts reach the sandbox on standard input, not the command line.
const Prompt = z.string().min(1).max(1_000_000);

export const StartAgent = z.object({
  prompt: Prompt,
  agent: AgentKind.default("claude"),
  // The directory to work in, default the home directory.
  cwd: z.string().min(1).max(4096).optional(),
  // The agent's model, such as opus or gpt-5; default the agent's own default.
  model: z.string().regex(/^[\w.:/@[\]-]{1,100}$/, "a model name").optional(),
});
export type StartAgent = z.input<typeof StartAgent>;

export const AgentMessage = z.object({
  prompt: Prompt,
  // Stop the running turn, and drop any queued messages, before this one.
  interrupt: z.boolean().default(false),
});
export type AgentMessage = z.input<typeof AgentMessage>;

export const AgentState = z.enum(["running", "idle"]);
export type AgentState = z.infer<typeof AgentState>;

export const Agent = z.object({
  id: z.string(),
  agent: AgentKind,
  // running while a turn is in progress or messages are queued, idle otherwise.
  state: AgentState,
  cwd: z.string(),
  model: z.string().nullable(),
  turns: z.number().int(), // turns started so far
  queued: z.number().int(), // messages waiting for the running turn
  events: z.number().int(), // events so far; the next one's seq is this plus 1
  // The agent's own conversation id: Claude Code's session or Codex's thread.
  sessionId: z.string().nullable(),
  // The agent's final message in the last finished turn, and how that turn ended.
  lastMessage: z.string().nullable(),
  lastExitCode: z.number().int().nullable(),
  error: z.string().nullable(),
  createdAt: z.number().int(),
  updatedAt: z.number().int(),
});
export type Agent = z.infer<typeof Agent>;

// One line of an agent's log. A turn is a prompt, then the agent's own JSON
// events (Claude Code's stream-json or Codex's exec --json) and any other
// output lines, then an end.
export const AgentEvent = z.object({
  seq: z.number().int(), // from 1
  turn: z.number().int(),
  at: z.number().int(),
  type: z.enum(["prompt", "event", "output", "end"]),
  text: z.string().optional(), // the prompt, or an output line that isn't JSON
  event: z.record(z.string(), z.unknown()).optional(), // the agent's event, as it printed it
  truncated: z.boolean().optional(), // a line over 256 KiB, cut to its first 4 KiB of text
  exitCode: z.number().int().optional(), // end: the agent's exit code
  interrupted: z.boolean().optional(), // end: the turn was interrupted, or the sandbox stopped
  error: z.string().nullable().optional(), // end: why the turn failed
});
export type AgentEvent = z.infer<typeof AgentEvent>;

export const AgentEvents = z.object({
  events: z.array(AgentEvent),
  next: z.number().int(), // pass as after to get the events that follow
  agent: Agent,
});
export type AgentEvents = z.infer<typeof AgentEvents>;

export const HostRequest = z.object({ port: z.number().int().min(1).max(65535) });
export const HostResult = z.object({ url: z.string() });
export type HostResult = z.infer<typeof HostResult>;

// The links handed out for a sandbox's ports since v0.1.14 that still work;
// older ones aren't listed, though they work until revoked. A revoked link
// stops working at once; asking for the port's link again gives a new one.
export const HostLink = z.object({ port: z.number().int(), url: z.string(), createdAt: z.number().int() });
export type HostLink = z.infer<typeof HostLink>;
export const RevokedLinks = z.object({
  revoked: z.array(z.number().int()), // ports whose links were listed
  all: z.boolean(), // every link to the sandbox, including ones made before links were listed
});
export type RevokedLinks = z.infer<typeof RevokedLinks>;

// The desktop's screen, from 640x480 up to 3840x2160. A running desktop is resized.
const ScreenSize = z
  .string()
  .regex(/^\d{3,4}x\d{3,4}$/, "a size such as 1280x720")
  .refine((s) => {
    const [w, h] = s.split("x").map(Number) as [number, number];
    return w >= 640 && w <= 3840 && h >= 480 && h <= 2160;
  }, "between 640x480 and 3840x2160");

export const DesktopRequest = z.object({ size: ScreenSize.default("1280x720") });
export type DesktopRequest = z.input<typeof DesktopRequest>;

// Where the desktop is served: the viewer page is <path> on guest <port>, which
// the TCP bridge or a preview link for that port reaches.
export const Desktop = z.object({
  port: z.number().int(),
  path: z.string(),
  size: z.string(),
  installed: z.boolean(), // installed by this call (the first one takes a minute or two)
  started: z.boolean(), // started by this call rather than already running
});
export type Desktop = z.infer<typeof Desktop>;

export const Health = z.object({
  ok: z.literal(true),
  version: z.string(),
  previewZone: z.string().nullable(),
  backups: z.boolean().optional(),
});
export type Health = z.infer<typeof Health>;

export const ApiError = z.object({ error: z.string() });
export type ApiError = z.infer<typeof ApiError>;
