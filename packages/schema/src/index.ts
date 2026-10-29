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

export const CreateSandbox = z.object({
  name: Name.optional(),
  size: SandboxSize.default("standard-2"),
  // Stop (and save) after this many minutes without SSH, exec, preview or agent
  // activity. 0 keeps it running until the platform's 6-hour limit.
  idleMinutes: z.number().int().min(0).max(360).default(30),
});
export type CreateSandbox = z.input<typeof CreateSandbox>;

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
export const ResumeRequest = z.object({ fromBackup: z.boolean().default(false) });
export type ResumeRequest = z.input<typeof ResumeRequest>;

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

export const HostRequest = z.object({ port: z.number().int().min(1).max(65535) });
export const HostResult = z.object({ url: z.string() });
export type HostResult = z.infer<typeof HostResult>;

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
