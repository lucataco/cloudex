// The Cloudex SDK: create, fork and drive sandboxes from TypeScript or
// JavaScript. It talks to the /v1 API that `cloudex deploy` set up in your
// Cloudflare account, and runs anywhere fetch does: Node, Bun, Deno and
// Workers. The API sends no CORS headers, so it isn't for browser pages. It has
// no dependencies.
//
// The types below mirror the models in @cloudex/schema; schema-check.ts makes
// any difference a type error.

export type SandboxSize = "standard-1" | "standard-2" | "standard-3" | "standard-4";
export type SandboxState = "starting" | "running" | "stopping" | "stopped" | "error";

/** A sandbox as the API reports it. Times are milliseconds since the epoch. */
export interface SandboxInfo {
  id: string;
  name: string | null;
  size: SandboxSize;
  state: SandboxState;
  /** Minutes without activity before it saves and stops; 0 waits for the 6-hour platform limit. */
  idleMinutes: number;
  createdAt: number;
  updatedAt: number;
  startedAt: number | null;
  stoppedAt: number | null;
  /** When the disk was last saved, and that save's size. */
  savedAt: number | null;
  savedBytes: number | null;
  /** The newest backup of /home in the account's R2 bucket. */
  backupAt?: number | null;
  backupBytes?: number | null;
  backupError?: string | null;
  /** Something to know about the last start, such as a restore from backup. */
  notice?: string | null;
  /** The id of the sandbox this one is a fork of. */
  forkedFrom?: string | null;
  /** The named save this one started from. */
  fromSave?: string | null;
  /** Providers whose keys this run uses without holding them (see Cloudex.setKey). */
  providerKeys?: string[];
  error: string | null;
}

/**
 * A named save: a copy of a sandbox's disk in the deployment's R2 bucket, kept
 * until it's deleted, that new sandboxes start from (Cloudex.startFromSave).
 * It holds every file that differs from the sandbox image, and the sandbox's
 * settings. Onto a newer image, only /home is restored.
 */
export interface SaveInfo {
  name: string;
  /** sv-xxxxxxxxxx, new each time the name is saved. */
  id: string;
  /** The id of the sandbox it was made from. */
  sandbox: string;
  /** disk: every file that differs from the image. home: /home only, from an image before v0.1.15. */
  kind: "disk" | "home";
  /** The defaults for sandboxes started from it. */
  size: SandboxSize;
  idleMinutes: number;
  /** Paths it holds, and their size. */
  files: number;
  rawBytes: number;
  /** Its size in R2, compressed. */
  bytes: number;
  createdAt: number;
  /** How long making it took. */
  ms: number;
  /** Only from Sandbox.save: whether it replaced an earlier save of the name. */
  replaced?: boolean;
}

/** An AI provider whose key the deployment can hold for sandboxes. */
export type Provider = "anthropic" | "openai" | "openrouter" | "google" | "groq" | "mistral" | "xai" | "deepseek";

/** A stored provider key. The key itself is never returned; hint is its last 4 characters. */
export interface ProviderKey {
  provider: Provider;
  hint: string;
  updatedAt: number;
}

export interface CreateOptions {
  /** 2-32 lowercase letters, digits and dashes, starting with a letter and not ending with a dash. Can't start with cx-, which marks sandbox ids. */
  name?: string;
  /** Default standard-2. */
  size?: SandboxSize;
  /** Default 30. */
  idleMinutes?: number;
  /** Environment variables every shell, ssh session, exec and agent gets, at every start. */
  env?: Record<string, string>;
  /** Files in the home directory (~/.npmrc or .npmrc), written at every start, mode 600. */
  files?: Record<string, string>;
  /** An https:// repo to clone into the home directory, with #branch to pick one. */
  repo?: string | RepoOptions;
}

export interface RepoOptions {
  /** HTTPS only. For a private GitHub repo, set GH_TOKEN or GITHUB_TOKEN in env. */
  url: string;
  /** Where in the home directory; default the repo's name. */
  dir?: string;
  branch?: string;
}

/** A change to the sandbox itself; fields left out stay as they are. */
export interface UpdateOptions {
  name?: string;
  /** Minutes without activity before it saves and stops, counting from now; 0 waits for the 6-hour limit. */
  idleMinutes?: number;
}

export interface ResumeOptions {
  /** Start a fresh image with /home restored from the newest backup, instead of the save. */
  fromBackup?: boolean;
  /** Run on this size from now on. The disk must fit the new size's. */
  size?: SandboxSize;
}

/** A preview link handed out that still works. */
export interface HostLink {
  port: number;
  url: string;
  createdAt: number;
}

/** What a revoke did: the ports whose links were listed, and whether every link to the sandbox went. */
export interface RevokedLinks {
  revoked: number[];
  all: boolean;
}

/** A sandbox's settings. Values and file contents never come back. */
export interface SandboxSettings {
  env: string[];
  /** As ~/path. */
  files: string[];
  repo: { url: string; dir: string; branch: string | null } | null;
}

/** A change to a sandbox's settings: null removes a variable or file. */
export interface SettingsChange {
  env?: Record<string, string | null>;
  files?: Record<string, string | null>;
}

export interface ForkOptions {
  /** The fork's name, or with count above 1, the start of <name>-1, <name>-2... */
  name?: string;
  /** 1 to 10; default 1. */
  count?: number;
  /** Default: the original's. */
  size?: SandboxSize;
  /** Default: the original's. */
  idleMinutes?: number;
}

export interface ExecOptions {
  /** Run as root instead of the cloudex user. */
  root?: boolean;
  /** Default: the user's home directory. A relative path, or one starting with ~, is in it. */
  cwd?: string;
  /** 1 to 3600; default 600. */
  timeoutSec?: number;
}

export interface ExecResult {
  exitCode: number;
  /** Each stream is cut at 1 MiB. */
  stdout: string;
  stderr: string;
  timedOut: boolean;
  ms: number;
}

/**
 * One line of a streamed exec: output as it comes, then exit. Output is UTF-8
 * text; bytes that aren't become U+FFFD, so read binary files with readFile.
 * keepalive comes every 15 seconds while the command prints nothing; error
 * ends a stream that broke off.
 */
export type ExecEvent =
  | { type: "stdout"; data: string }
  | { type: "stderr"; data: string }
  | { type: "keepalive" }
  | { type: "exit"; exitCode: number; timedOut: boolean; ms: number }
  | { type: "error"; error: string };

/** What Sandbox.execStream yields: keepalives are skipped, and an error is thrown. */
export type ExecStreamEvent = Extract<ExecEvent, { type: "stdout" | "stderr" | "exit" }>;

export interface CommandOptions {
  /** Run as root instead of the cloudex user. */
  root?: boolean;
  /** Default: the user's home directory. A relative path, or one starting with ~, is in it. */
  cwd?: string;
  /** Stop it after this long; 0, the default, never. */
  timeoutSec?: number;
}

/**
 * running; exited with exitCode; killed by a signal, which includes a kill
 * and a timeout; or lost: the sandbox stopped, or was forked or saved, while
 * it ran.
 */
export type CommandState = "running" | "exited" | "killed" | "lost";
export type CommandSignal = "TERM" | "INT" | "HUP" | "KILL";

/** A background command. Times are milliseconds since the epoch. */
export interface Command {
  /** cm-xxxxxxxx */
  id: string;
  cmd: string;
  root: boolean;
  cwd: string;
  state: CommandState;
  exitCode: number | null;
  /** The signal that ended it, such as TERM or KILL. */
  signal: string | null;
  timedOut: boolean;
  timeoutSec: number;
  startedAt: number;
  endedAt: number | null;
  /** The size of its output so far. */
  outputBytes: number;
}

/**
 * Part of a command's output (standard output and error together), from byte
 * offset `offset` to `next`, as UTF-8 text. Pass next as after to read on.
 */
export interface CommandOutput {
  data: string;
  offset: number;
  next: number;
  /** The command as of this read. */
  command: Command;
}

export type FileType = "file" | "dir" | "symlink" | "other";

/** One entry in a directory. mtime is milliseconds since the epoch. */
export interface FileEntry {
  name: string;
  type: FileType;
  size: number;
  /** Permission bits in octal, such as 644. */
  mode: string;
  mtime: number;
  /** A symlink's target. */
  target?: string;
}

/** A path's details and, for a directory, what's in it. */
export interface FileInfo {
  /** Absolute. */
  path: string;
  type: FileType;
  size: number;
  /** Permission bits in octal, such as 644. */
  mode: string;
  mtime: number;
  /** A symlink's target. */
  target?: string;
  /** A directory's entries, sorted by name, at most 10,000. */
  entries?: FileEntry[];
  /** The directory had more entries than these. */
  truncated?: boolean;
}

/** What an archive upload unpacked, and where. */
export interface ArchiveResult {
  path: string;
  entries: number;
}

/** Bytes to upload. A string is sent as UTF-8. */
export type FileData = string | Uint8Array | ArrayBuffer | Blob | ReadableStream<Uint8Array>;

export interface FileOptions {
  /** Act as root instead of the cloudex user. */
  root?: boolean;
}

export interface Desktop {
  /** The guest port serving the viewer. */
  port: number;
  /** The viewer page on that port, with its options. */
  path: string;
  size: string;
  /** Whether this call installed it (the first start takes a minute or two). */
  installed: boolean;
  /** Whether this call started it, rather than finding it running. */
  started: boolean;
}

export type AgentKind = "claude" | "codex" | "opencode";
export type AgentState = "running" | "idle";

export interface PromptOptions {
  /** Default claude. */
  agent?: AgentKind;
  /** The directory to work in; default the home directory. */
  cwd?: string;
  /** The agent's model, such as opus or gpt-5; default the agent's own. */
  model?: string;
}

/** An agent as the API reports it. Times are milliseconds since the epoch. */
export interface AgentInfo {
  id: string;
  agent: AgentKind;
  /** running while a turn is in progress or messages are queued. */
  state: AgentState;
  cwd: string;
  model: string | null;
  /** Turns started so far. */
  turns: number;
  /** Messages waiting for the running turn. */
  queued: number;
  /** Events so far. */
  events: number;
  /** The agent's own conversation id: Claude Code's or OpenCode's session, or Codex's thread. */
  sessionId: string | null;
  /** The agent's final message in the last finished turn. */
  lastMessage: string | null;
  lastExitCode: number | null;
  /** Why the last turn failed. */
  error: string | null;
  createdAt: number;
  updatedAt: number;
}

/**
 * One line of an agent's log. A turn is a prompt, then the agent's own JSON
 * events (Claude Code's stream-json, Codex's exec --json or OpenCode's run --format json) and any other
 * output lines, then an end.
 */
export interface AgentEvent {
  /** From 1. */
  seq: number;
  turn: number;
  at: number;
  type: "prompt" | "event" | "output" | "end";
  /** The prompt, or an output line that isn't JSON. */
  text?: string;
  /** The agent's event, as it printed it. */
  event?: Record<string, unknown>;
  /** A line over 256 KiB, cut to its first 4 KiB of text. */
  truncated?: boolean;
  /** end: the agent's exit code. */
  exitCode?: number;
  /** end: the turn was interrupted, or the sandbox stopped. */
  interrupted?: boolean;
  /** end: why the turn failed. */
  error?: string | null;
}

export interface AgentEvents {
  events: AgentEvent[];
  /** Pass as after to get the events that follow. */
  next: number;
  agent: AgentInfo;
}

export interface Health {
  ok: true;
  version: string;
  previewZone: string | null;
  backups?: boolean;
}

export interface CloudexOptions {
  /** Default: $CLOUDEX_API_URL, then the CLI's saved deployment. */
  apiUrl?: string;
  /** Default: $CLOUDEX_API_KEY, then the CLI's saved key, but only if apiUrl is the saved deployment's. */
  apiKey?: string;
  /** A fetch to use instead of the global one. */
  fetch?: (input: string, init: RequestInit) => Promise<Response>;
}

/** An error from the API, with its HTTP status (0 when there was no request, or a stream broke off). */
export class CloudexError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "CloudexError";
    this.status = status;
  }
}

type Call = <T>(method: string, path: string, body?: unknown) => Promise<T>;
type Send = (method: string, path: string, body?: FileData, contentType?: string) => Promise<Response>;

export class Cloudex {
  #options: CloudexOptions;
  #endpoint: Promise<{ url: string; key: string }> | undefined;
  #fetch: NonNullable<CloudexOptions["fetch"]>;

  constructor(options: CloudexOptions = {}) {
    this.#options = options;
    // Wrapped, because some runtimes reject fetch called on another object.
    this.#fetch = options.fetch ?? ((input, init) => fetch(input, init));
  }

  /** The deployment's version and features. */
  health(): Promise<Health> {
    return this.#call("GET", "/v1/health");
  }

  /** All sandboxes, newest first, as a summary: get() or refresh() has every field (notice, forkedFrom, fromSave, providerKeys, backup*). */
  async list(): Promise<Sandbox[]> {
    const list = await this.#call<SandboxInfo[]>("GET", "/v1/sandboxes");
    return list.map((info) => this.#wrap(info));
  }

  /** A sandbox by id (cx-xxxxxx) or name. */
  async get(ref: string): Promise<Sandbox> {
    return this.#wrap(await this.#call<SandboxInfo>("GET", sandboxPath(ref)));
  }

  /** Creates a sandbox and answers once it's running, usually in about 10 seconds. */
  async create(options: CreateOptions = {}): Promise<Sandbox> {
    return this.#wrap(await this.#call<SandboxInfo>("POST", "/v1/sandboxes", options));
  }

  /** The AI provider keys sandboxes use; the keys themselves are never returned. */
  listKeys(): Promise<ProviderKey[]> {
    return this.#call("GET", "/v1/keys");
  }

  /**
   * Sets a provider's key. Sandboxes started from then on get a placeholder,
   * and the real key is added to their HTTPS calls to the provider on the way
   * out, so it never enters a sandbox, its saves, backups or forks. A changed
   * key reaches running sandboxes within a minute. The key is tried with the
   * provider first; if the provider rejects it, this throws a CloudexError
   * (status 400) with what the provider said.
   */
  setKey(provider: Provider, key: string): Promise<ProviderKey> {
    return this.#call("PUT", `/v1/keys/${provider}`, { key });
  }

  async deleteKey(provider: Provider): Promise<void> {
    await this.#call("DELETE", `/v1/keys/${provider}`);
  }

  /** The named saves, newest first. Make one with Sandbox.save. */
  listSaves(): Promise<SaveInfo[]> {
    return this.#call("GET", "/v1/saves");
  }

  getSave(name: string): Promise<SaveInfo> {
    return this.#call("GET", savePath(name));
  }

  /** Deletes a named save for good. Sandboxes started from it keep their disks. */
  async deleteSave(name: string): Promise<void> {
    await this.#call("DELETE", savePath(name));
  }

  /**
   * Starts sandboxes from a named save, with the same options as a fork; size
   * and idle minutes default to the saved sandbox's. Restoring takes about 15
   * seconds per GB. Ones that failed to start come back in state "error";
   * resume() tries again.
   */
  async startFromSave(name: string, options: ForkOptions = {}): Promise<Sandbox[]> {
    const list = await this.#call<SandboxInfo[]>("POST", `${savePath(name)}/sandboxes`, options);
    return list.map((info) => this.#wrap(info));
  }

  #wrap(info: SandboxInfo) {
    return new Sandbox(info, this.#call, this.#send);
  }

  #call: Call = async <T>(method: string, path: string, body?: unknown): Promise<T> => {
    const json = body === undefined ? undefined : JSON.stringify(body);
    return readJson<T>(await this.#send(method, path, json, json === undefined ? undefined : "application/json"));
  };

  // A request, answered with the response unread; an error status throws.
  #send: Send = async (method, path, body, contentType) => {
    const { url, key } = await (this.#endpoint ??= this.#resolve());
    const init: RequestInit & { duplex?: "half" } = {
      method,
      headers: { authorization: `Bearer ${key}`, ...(contentType ? { "content-type": contentType } : {}) },
      body: body as RequestInit["body"],
    };
    // Node and Deno send a stream only when told it's one-way.
    if (typeof ReadableStream !== "undefined" && body instanceof ReadableStream) init.duplex = "half";
    let res: Response;
    try {
      res = await this.#fetch(url + path, init);
    } catch (e) {
      throw unreachable(url, e);
    }
    if (!res.ok) {
      const text = await readText(res);
      let error: unknown;
      try {
        error = (JSON.parse(text) as { error?: unknown }).error;
      } catch {
        error = undefined;
      }
      const message = typeof error === "string" ? error : plainError(res.status, text) || res.statusText;
      throw new CloudexError(res.status, res.status === 404 && message === "not found" ? OLDER_DEPLOYMENT : message);
    }
    return res;
  };

  // The same order as the CLI: options, then the environment, then the
  // deployment saved by `cloudex deploy` or `cloudex login`. The saved key goes
  // only to the saved URL, so a URL passed in can't be sent the admin key.
  async #resolve() {
    const env = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env ?? {};
    let url = this.#options.apiUrl ?? env.CLOUDEX_API_URL;
    let key = this.#options.apiKey ?? env.CLOUDEX_API_KEY;
    if (!url || !key) {
      const saved = await savedDeployment(env.CLOUDEX_CONFIG_DIR);
      url ||= saved?.apiUrl;
      if (!key && url && saved?.apiUrl && normalizeUrl(url) === normalizeUrl(saved.apiUrl)) key = saved.apiKey;
    }
    url = normalizeUrl(url ?? "");
    // Let a later call find a deployment or key set up since.
    if (!url) {
      this.#endpoint = undefined;
      throw new CloudexError(
        0,
        "no Cloudex deployment: pass apiUrl and apiKey, set CLOUDEX_API_URL and CLOUDEX_API_KEY, or run cloudex login",
      );
    }
    if (!key) {
      this.#endpoint = undefined;
      throw new CloudexError(
        0,
        `no API key for ${url}: pass apiKey or set CLOUDEX_API_KEY. The CLI's saved key is used only with the deployment it was saved for`,
      );
    }
    return { url, key };
  }
}

/** A sandbox. Its fields are as of the last call that returned it; refresh() updates them. */
export class Sandbox {
  #info: SandboxInfo;
  #call: Call;
  #send: Send;

  /** @internal Get sandboxes from Cloudex.create, get, list or fork. */
  constructor(info: SandboxInfo, call: Call, send: Send) {
    this.#info = info;
    this.#call = call;
    this.#send = send;
  }

  get id(): string {
    return this.#info.id;
  }
  get name(): string | null {
    return this.#info.name;
  }
  get state(): SandboxState {
    return this.#info.state;
  }
  get info(): SandboxInfo {
    return this.#info;
  }

  toJSON(): SandboxInfo {
    return this.#info;
  }

  async refresh(): Promise<this> {
    return this.#update("GET", "");
  }

  /** Saves the disk, backs up /home and stops. */
  async stop(): Promise<this> {
    return this.#update("POST", "/stop");
  }

  /** Starts from the save, or with fromBackup, from a fresh image with /home restored; with size, on another size. */
  async resume(options: ResumeOptions = {}): Promise<this> {
    return this.#update("POST", "/resume", options);
  }

  /** Renames the sandbox, or changes how long it may sit idle; a new idle time applies at once. */
  async update(options: UpdateOptions): Promise<this> {
    return this.#update("PATCH", "", options);
  }

  /** Deletes the sandbox and its backups. */
  async delete(): Promise<void> {
    await this.#call("DELETE", this.#path(""));
  }

  /**
   * Runs a command with `bash -lc` and waits for it. A string is run as shell
   * code; an array is quoted, one argument per element. A failing command
   * doesn't throw: check exitCode. It reads the API's stream of output, which
   * keeps a long command from hitting Node's five-minute wait for a response.
   */
  async exec(command: string | string[], options: ExecOptions = {}): Promise<ExecResult> {
    const res = await this.#startExec(command, options);
    if (!isStream(res)) return readJson<ExecResult>(res);
    const stdout = new Clipped();
    const stderr = new Clipped();
    for await (const e of execEvents(res)) {
      if (e.type === "stdout") stdout.add(e.data);
      else if (e.type === "stderr") stderr.add(e.data);
      else return { exitCode: e.exitCode, stdout: stdout.text(), stderr: stderr.text(), timedOut: e.timedOut, ms: e.ms };
    }
    throw new CloudexError(0, "the exec stream ended before the command did");
  }

  /**
   * Runs a command like exec, yielding its output as it comes: stdout and
   * stderr events, then an exit event with the exit code. Streamed output
   * isn't cut. Leaving the loop early stops the command. If the stream breaks
   * off it throws CloudexError (status 0); the command may still be running.
   * An API before v0.1.16 answers once the command ends, so all its output
   * comes at once.
   */
  async *execStream(
    command: string | string[],
    options: ExecOptions = {},
  ): AsyncGenerator<ExecStreamEvent, void, undefined> {
    const res = await this.#startExec(command, options);
    if (!isStream(res)) {
      const r = await readJson<ExecResult>(res);
      if (r.stdout) yield { type: "stdout", data: r.stdout };
      if (r.stderr) yield { type: "stderr", data: r.stderr };
      yield { type: "exit", exitCode: r.exitCode, timedOut: r.timedOut, ms: r.ms };
      return;
    }
    yield* execEvents(res);
  }

  // An API before v0.1.16 ignores stream and answers once, with JSON.
  #startExec(command: string | string[], options: ExecOptions) {
    const body = JSON.stringify({ cmd: commandLine(command), ...options, stream: true });
    return this.#send("POST", this.#path("/exec"), body, "application/json");
  }

  /**
   * Starts a command in the background and answers once it's running. A string
   * is run as shell code with `bash -lc`; an array is quoted. Its output
   * (stdout and stderr together) is kept on the sandbox's disk: read it with
   * commandOutput or followCommand. A stop ends it.
   */
  startCommand(command: string | string[], options: CommandOptions = {}): Promise<Command> {
    return this.#call("POST", this.#path("/commands"), { cmd: commandLine(command), ...options });
  }

  /** The sandbox's background commands, newest first. */
  listCommands(): Promise<Command[]> {
    return this.#call("GET", this.#path("/commands"));
  }

  /** A background command by id (cm-xxxxxxxx). */
  getCommand(id: string): Promise<Command> {
    return this.#call("GET", this.#commandPath(id));
  }

  /**
   * A command's output from byte offset after (default 0), up to 1 MiB. With
   * wait (seconds, at most 60), it waits for output after that or for the
   * command to end. Pass next as after to read on.
   */
  commandOutput(id: string, options: { after?: number; wait?: number } = {}): Promise<CommandOutput> {
    return this.#call("GET", `${this.#commandPath(id)}/output?after=${options.after ?? 0}&wait=${options.wait ?? 0}`);
  }

  /**
   * Follows a command's output from after (default 0) until it ends and all
   * of it is read. Each item is a piece of output; the last one's command
   * tells how it ended.
   */
  async *followCommand(id: string, options: { after?: number } = {}): AsyncGenerator<CommandOutput> {
    let after = options.after ?? 0;
    // Once the command has ended, read only what its log held then, so that
    // something it left running can't keep this going.
    let end = -1;
    for (;;) {
      const page = await this.commandOutput(id, { after, wait: 30 });
      if (page.command.state !== "running" && end < 0) end = page.command.outputBytes;
      const ended = end >= 0 && (page.next >= end || page.next === after);
      if (page.data || ended) yield page;
      if (ended) return;
      after = page.next;
    }
  }

  /** Sends a running command, and everything it started, a signal (default TERM). */
  killCommand(id: string, signal?: CommandSignal): Promise<Command> {
    return this.#call("POST", `${this.#commandPath(id)}/kill`, signal ? { signal } : {});
  }

  /** Deletes a finished command's record and output; a running one must be killed first. */
  forgetCommand(id: string): Promise<Command> {
    return this.#call("DELETE", this.#commandPath(id));
  }

  /**
   * A path's details and, for a directory, what's in it. A relative path, or
   * one starting with ~, is in the home directory.
   */
  stat(path: string, options: FileOptions = {}): Promise<FileInfo> {
    return this.#call("GET", this.#filePath("", path, options));
  }

  /** A file's contents. */
  async readFile(path: string, options: FileOptions = {}): Promise<Uint8Array> {
    const res = await this.#send("GET", this.#filePath("/content", path, options));
    return readBytes(res);
  }

  /**
   * Writes a file whole, making its directory if need be. It keeps its mode
   * unless mode is given, in octal: "755" or 0o755. Cloudflare limits a
   * request to 100 MB on most plans.
   */
  async writeFile(path: string, data: FileData, options: FileOptions & { mode?: string | number } = {}): Promise<FileInfo> {
    // The API wants 3 or 4 octal digits.
    const mode = typeof options.mode === "number" ? options.mode.toString(8).padStart(3, "0") : options.mode;
    const res = await this.#send("PUT", this.#filePath("/content", path, { ...options, mode }), data, "application/octet-stream");
    return readJson(res);
  }

  /** Deletes a file, or with recursive, a directory and everything in it. Returns what was deleted. */
  deleteFile(path: string, options: FileOptions & { recursive?: boolean } = {}): Promise<FileInfo> {
    return this.#call("DELETE", this.#filePath("", path, options));
  }

  /** A directory's contents, or one file, as a tar.gz. Files the user can't read are left out. */
  async downloadArchive(path: string, options: FileOptions = {}): Promise<Uint8Array> {
    return readBytes(await this.#send("GET", this.#filePath("/archive", path, options)));
  }

  /** Unpacks a tar.gz into a directory, making it if need be. */
  async uploadArchive(path: string, data: FileData, options: FileOptions = {}): Promise<ArchiveResult> {
    return readJson(await this.#send("PUT", this.#filePath("/archive", path, options), data, "application/gzip"));
  }

  /**
   * Makes sandboxes that start from a copy of this one's whole disk. A running
   * sandbox is saved first, in seconds, and keeps running. Forks that failed
   * to start come back in state "error"; resume() tries again.
   */
  async fork(options: ForkOptions = {}): Promise<Sandbox[]> {
    const forks = await this.#call<SandboxInfo[]>("POST", this.#path("/fork"), options);
    return forks.map((info) => new Sandbox(info, this.#call, this.#send));
  }

  /**
   * Saves this running sandbox's disk under a name, to start new sandboxes
   * from with Cloudex.startFromSave. Unlike its own save, a named save stays
   * until it's deleted. The sandbox keeps running; saving takes about 15
   * seconds per GB of changes. A taken name's save is replaced.
   */
  save(name: string): Promise<SaveInfo> {
    return this.#call("POST", this.#path("/saves"), { name });
  }

  /**
   * An HTTPS link to a port, on the deployment's preview domain. Anyone with it
   * can use it. The same port gives the same link until it's revoked. Servers
   * that listen only on localhost work too.
   */
  async host(port: number): Promise<string> {
    const { url } = await this.#call<{ url: string }>("POST", this.#path("/hosts"), { port });
    return url;
  }

  /** The preview links handed out since v0.1.14 that still work; older ones aren't listed, and revokeLinks() ends them too. */
  links(): Promise<HostLink[]> {
    return this.#call("GET", this.#path("/hosts"));
  }

  /**
   * Makes a port's link stop working at once, or without a port, every link to
   * the sandbox, the desktop's too. host() then gives a new link.
   */
  revokeLinks(port?: number): Promise<RevokedLinks> {
    return this.#call("DELETE", this.#path(port === undefined ? "/hosts" : `/hosts/${port}`));
  }

  /**
   * Starts the desktop (Chrome and a terminal), or resizes a running one, and
   * returns a link to its viewer on the preview domain, so the deployment needs
   * one. The first start installs it, which takes a minute or two.
   */
  async desktop(options: { size?: string } = {}): Promise<Desktop & { url: string }> {
    // Checked first: starting the desktop without a way to link to it would only fail afterwards.
    if (!(await this.#call<Health>("GET", "/v1/health")).previewZone) {
      throw new CloudexError(400, "preview links need a domain: run cloudex deploy --preview-domain <your domain>");
    }
    const d = await this.#call<Desktop>("POST", this.#path("/desktop"), options);
    return { ...d, url: `${await this.host(d.port)}/${d.path}` };
  }

  async stopDesktop(): Promise<void> {
    await this.#call("DELETE", this.#path("/desktop"));
  }

  /** The sandbox's environment variable names, files and repo. */
  settings(): Promise<SandboxSettings> {
    return this.#call("GET", this.#path("/settings"));
  }

  /**
   * Sets or removes (null) environment variables and files. A running sandbox
   * gets the change at once, in new shells; a stopped one when it next starts.
   */
  updateSettings(change: SettingsChange): Promise<SandboxSettings> {
    return this.#call("PATCH", this.#path("/settings"), change);
  }

  /**
   * Starts Claude Code (or Codex or OpenCode) on a task, with no terminal, and answers at
   * once. Follow it with agent.events() or agent.wait(), and continue the
   * conversation with agent.send(). With provider keys set, it needs no sign-in.
   */
  async prompt(prompt: string, options: PromptOptions = {}): Promise<Agent> {
    const info = await this.#call<AgentInfo>("POST", this.#path("/agents"), { prompt, ...options });
    return new Agent(info, this.#path(`/agents/${info.id}`), this.#call);
  }

  /** The agents started in this sandbox, newest first. */
  async agents(): Promise<Agent[]> {
    const list = await this.#call<AgentInfo[]>("GET", this.#path("/agents"));
    return list.map((info) => new Agent(info, this.#path(`/agents/${info.id}`), this.#call));
  }

  /** An agent by id (ag-xxxxxxxx). */
  async agent(id: string): Promise<Agent> {
    const path = this.#path(`/agents/${encodeURIComponent(id)}`);
    return new Agent(await this.#call<AgentInfo>("GET", path), path, this.#call);
  }

  #path(suffix: string) {
    return sandboxPath(this.#info.id) + suffix;
  }

  #commandPath(id: string) {
    return this.#path(`/commands/${encodeURIComponent(id)}`);
  }

  #filePath(suffix: string, path: string, options: { root?: boolean; recursive?: boolean; mode?: string }) {
    let q = `?path=${encodeURIComponent(path).replaceAll("%2F", "/")}`;
    if (options.root) q += "&root=true";
    if (options.recursive) q += "&recursive=true";
    if (options.mode) q += `&mode=${encodeURIComponent(options.mode)}`;
    return this.#path(`/files${suffix}${q}`);
  }

  async #update(method: string, suffix: string, body?: unknown): Promise<this> {
    this.#info = await this.#call<SandboxInfo>(method, this.#path(suffix), body);
    return this;
  }
}

/**
 * Claude Code, Codex or OpenCode working on a task in a sandbox. Its fields are as of the
 * last call that returned it; refresh(), events() and wait() update them.
 */
export class Agent {
  #info: AgentInfo;
  #path: string;
  #call: Call;

  /** @internal Get agents from Sandbox.prompt, agents or agent. */
  constructor(info: AgentInfo, path: string, call: Call) {
    this.#info = info;
    this.#path = path;
    this.#call = call;
  }

  get id(): string {
    return this.#info.id;
  }
  get state(): AgentState {
    return this.#info.state;
  }
  /** The agent's final message in the last finished turn. */
  get lastMessage(): string | null {
    return this.#info.lastMessage;
  }
  /** Why the last turn failed. */
  get error(): string | null {
    return this.#info.error;
  }
  get info(): AgentInfo {
    return this.#info;
  }

  toJSON(): AgentInfo {
    return this.#info;
  }

  async refresh(): Promise<this> {
    this.#info = await this.#call<AgentInfo>("GET", this.#path);
    return this;
  }

  /**
   * Continues the conversation. While a turn runs the message waits for it;
   * with interrupt, the running turn stops and queued messages are dropped first.
   */
  async send(prompt: string, options: { interrupt?: boolean } = {}): Promise<this> {
    this.#info = await this.#call<AgentInfo>("POST", `${this.#path}/messages`, { prompt, ...options });
    return this;
  }

  /** Stops the running turn and drops queued messages. */
  async interrupt(): Promise<this> {
    this.#info = await this.#call<AgentInfo>("POST", `${this.#path}/interrupt`);
    return this;
  }

  /**
   * The agent's events from after (default 0, the start). With follow (the
   * default), it waits for new ones until the agent is idle; without, it stops
   * at the newest.
   */
  async *events(options: { after?: number; follow?: boolean } = {}): AsyncGenerator<AgentEvent> {
    const follow = options.follow ?? true;
    let after = options.after ?? 0;
    for (;;) {
      const page = await this.#call<AgentEvents>("GET", `${this.#path}/events?after=${after}&wait=${follow ? 30 : 0}`);
      this.#info = page.agent;
      yield* page.events;
      after = page.next;
      if (after >= page.agent.events && (!follow || page.agent.state === "idle")) return;
    }
  }

  /** Waits until the agent is idle; then lastMessage and error tell how its last turn went. */
  async wait(): Promise<this> {
    for await (const _ of this.events({ after: this.#info.events })) {
      // Only the end matters.
    }
    return this;
  }
}

const sandboxPath = (ref: string) => `/v1/sandboxes/${encodeURIComponent(ref)}`;
const savePath = (name: string) => `/v1/saves/${encodeURIComponent(name)}`;

const commandLine = (command: string | string[]) =>
  typeof command === "string" ? command : command.map(shellQuote).join(" ");

const OLDER_DEPLOYMENT = "this deployment is older than the SDK and doesn't have that call; update it with: cloudex deploy";

// Must agree with the CLI's NormalizeURL, so that the saved key matches its
// deployment however either URL is written.
function normalizeUrl(url: string) {
  let u = url.trim().replace(/\/+$/, "");
  if (!u) return u;
  if (!u.includes("://")) u = `https://${u}`;
  const at = u.indexOf("://");
  const scheme = u.slice(0, at).toLowerCase();
  const rest = u.slice(at + 3);
  const slash = rest.includes("/") ? rest.indexOf("/") : rest.length;
  const host = rest.slice(0, slash).toLowerCase();
  return `${scheme}://${scheme === "https" ? host.replace(/:443$/, "") : host}${rest.slice(slash)}`;
}

// A failed fetch or a body that broke off. Node's fetch says only "fetch failed"
// or "terminated"; the reason is in cause.
function unreachable(url: string, e: unknown): CloudexError {
  const cause = e instanceof Error && e.cause instanceof Error ? e.cause : e;
  const reason = cause instanceof Error ? cause.message || (cause as { code?: string }).code : String(cause);
  return new CloudexError(0, `can't reach ${url}: ${reason || "no answer"}`);
}

const origin = (res: Response) => (res.url ? new URL(res.url).origin : "the API");

async function readBody<T>(res: Response, read: () => Promise<T>): Promise<T> {
  try {
    return await read();
  } catch (e) {
    throw unreachable(origin(res), e);
  }
}

const readText = (res: Response) => readBody(res, () => res.text());
const readBytes = async (res: Response) => new Uint8Array(await readBody(res, () => res.arrayBuffer()));

const htmlTitle = /<title[^>]*>([\s\S]*?)<\/title>/i;

// An answer that isn't the API's JSON, such as Cloudflare's HTML error page,
// becomes its status and title; other text is cut at 500 characters.
function plainError(status: number, body: string): string {
  body = body.trim();
  if (body.startsWith("<")) {
    const title = (htmlTitle.exec(body)?.[1] ?? "").split(/\s+/).filter(Boolean).join(" ");
    return title.startsWith(String(status)) ? title : `${status} ${title}`.trim();
  }
  const chars = [...body];
  return chars.length > 500 ? `${chars.slice(0, 500).join("")}…` : body;
}

async function readJson<T>(res: Response): Promise<T> {
  const text = await readText(res);
  return (text ? JSON.parse(text) : undefined) as T;
}

const isStream = (res: Response) => Boolean(res.headers.get("content-type")?.includes("ndjson"));

async function* execEvents(res: Response): AsyncGenerator<ExecStreamEvent> {
  for await (const line of lines(res)) {
    const event = JSON.parse(line) as ExecEvent;
    if (event.type === "error") throw new CloudexError(0, event.error);
    if (event.type === "stdout" || event.type === "stderr" || event.type === "exit") yield event;
    if (event.type === "exit") return;
  }
  throw new CloudexError(0, "the exec stream ended before the command did");
}

const OUTPUT_LIMIT = 1024 * 1024;

// One stream of an exec's output, cut the way the API cuts it: the first
// 1 MiB, then a note with the full size.
class Clipped {
  #text = "";
  #kept = 0;
  #total = 0;

  add(data: string) {
    const bytes = new TextEncoder().encode(data);
    this.#total += bytes.length;
    const room = OUTPUT_LIMIT - this.#kept;
    if (room <= 0) return;
    this.#text += bytes.length <= room ? data : new TextDecoder().decode(bytes.subarray(0, room));
    this.#kept += Math.min(bytes.length, room);
  }

  text() {
    return this.#total > OUTPUT_LIMIT ? `${this.#text}\n[cloudex: output cut at 1 MiB of ${this.#total} bytes]` : this.#text;
  }
}

// The lines of a response, as they come. Stopping early cancels the response.
async function* lines(res: Response): AsyncGenerator<string> {
  if (!res.body) return;
  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let rest = "";
  try {
    for (;;) {
      const { done, value } = await readBody(res, () => reader.read());
      rest += done ? decoder.decode() : decoder.decode(value, { stream: true });
      const parts = rest.split("\n");
      rest = done ? "" : parts.pop()!;
      for (const line of parts) if (line.trim()) yield line;
      if (done) return;
    }
  } finally {
    await reader.cancel().catch(() => {});
  }
}

/** Quotes one argument for bash. */
export function shellQuote(arg: string): string {
  return /^[\w@%+=:,./-]+$/.test(arg) ? arg : `'${arg.replaceAll("'", `'\\''`)}'`;
}

// The CLI keeps its deployment in ~/.config/cloudex/config.json. Only Node-like
// runtimes have a file system; the module name is a variable so that bundlers
// for Workers leave it alone.
async function savedDeployment(dir?: string): Promise<{ apiUrl?: string; apiKey?: string } | undefined> {
  const load = (name: string) => import(/* @vite-ignore */ name);
  try {
    const [{ readFile }, { homedir }] = await Promise.all([load("node:fs/promises"), load("node:os")]);
    const text = await readFile(`${dir || `${homedir()}/.config/cloudex`}/config.json`, "utf8");
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}
