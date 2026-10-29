// The Cloudex SDK: create, fork and drive sandboxes from TypeScript or
// JavaScript. It talks to the /v1 API that `cloudex deploy` set up in your
// Cloudflare account, and runs anywhere fetch does: Node, Bun, Deno, Workers
// and browsers. It has no dependencies.
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
  /** Providers whose keys this run uses without holding them (see Cloudex.setKey). */
  providerKeys?: string[];
  error: string | null;
}

/** An AI provider whose key the deployment can hold for sandboxes. */
export type Provider = "anthropic" | "openai";

/** A stored provider key. The key itself is never returned; hint is its last 4 characters. */
export interface ProviderKey {
  provider: Provider;
  hint: string;
  updatedAt: number;
}

export interface CreateOptions {
  /** Lowercase letters, digits and dashes; 2-32 characters. */
  name?: string;
  /** Default standard-2. */
  size?: SandboxSize;
  /** Default 30. */
  idleMinutes?: number;
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
  /** Default: the user's home directory. */
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

export interface Health {
  ok: true;
  version: string;
  previewZone: string | null;
  backups?: boolean;
}

export interface CloudexOptions {
  /** Default: $CLOUDEX_API_URL, then the CLI's saved deployment. */
  apiUrl?: string;
  /** Default: $CLOUDEX_API_KEY, then the CLI's saved deployment. */
  apiKey?: string;
  /** A fetch to use instead of the global one. */
  fetch?: (input: string, init: RequestInit) => Promise<Response>;
}

/** An error from the API, with its HTTP status (0 when there was no request). */
export class CloudexError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "CloudexError";
    this.status = status;
  }
}

type Call = <T>(method: string, path: string, body?: unknown) => Promise<T>;

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

  /** All sandboxes, newest first. */
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
   * key reaches running sandboxes within a minute.
   */
  setKey(provider: Provider, key: string): Promise<ProviderKey> {
    return this.#call("PUT", `/v1/keys/${provider}`, { key });
  }

  async deleteKey(provider: Provider): Promise<void> {
    await this.#call("DELETE", `/v1/keys/${provider}`);
  }

  #wrap(info: SandboxInfo) {
    return new Sandbox(info, (method, path, body) => this.#call(method, path, body));
  }

  #call: Call = async <T>(method: string, path: string, body?: unknown): Promise<T> => {
    const { url, key } = await (this.#endpoint ??= this.#resolve());
    const res = await this.#fetch(url + path, {
      method,
      headers: {
        authorization: `Bearer ${key}`,
        ...(body === undefined ? {} : { "content-type": "application/json" }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const text = await res.text();
    let data: unknown;
    try {
      data = text ? JSON.parse(text) : undefined;
    } catch {
      data = undefined;
    }
    if (!res.ok) {
      const error = (data as { error?: unknown } | undefined)?.error;
      throw new CloudexError(res.status, typeof error === "string" ? error : text.trim() || res.statusText);
    }
    return data as T;
  };

  // The same order as the CLI: options, then the environment, then the
  // deployment saved by `cloudex deploy` or `cloudex login`.
  async #resolve() {
    const env = (globalThis as { process?: { env?: Record<string, string | undefined> } }).process?.env ?? {};
    let url = this.#options.apiUrl ?? env.CLOUDEX_API_URL;
    let key = this.#options.apiKey ?? env.CLOUDEX_API_KEY;
    if (!url || !key) {
      const saved = await savedDeployment(env.CLOUDEX_CONFIG_DIR);
      url ||= saved?.apiUrl;
      key ||= saved?.apiKey;
    }
    if (!url) {
      this.#endpoint = undefined; // let a later call find a deployment set up since
      throw new CloudexError(
        0,
        "no Cloudex deployment: pass apiUrl and apiKey, set CLOUDEX_API_URL and CLOUDEX_API_KEY, or run cloudex login",
      );
    }
    return { url: url.replace(/\/+$/, ""), key: key ?? "" };
  }
}

/** A sandbox. Its fields are as of the last call that returned it; refresh() updates them. */
export class Sandbox {
  #info: SandboxInfo;
  #call: Call;

  /** @internal Get sandboxes from Cloudex.create, get, list or fork. */
  constructor(info: SandboxInfo, call: Call) {
    this.#info = info;
    this.#call = call;
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

  /** Starts from the save, or with fromBackup, from a fresh image with /home restored. */
  async resume(options: { fromBackup?: boolean } = {}): Promise<this> {
    return this.#update("POST", "/resume", options);
  }

  /** Deletes the sandbox and its backups. */
  async delete(): Promise<void> {
    await this.#call("DELETE", this.#path(""));
  }

  /**
   * Runs a command with `bash -lc` and waits for it. A string is run as shell
   * code; an array is quoted, one argument per element. A failing command
   * doesn't throw: check exitCode.
   */
  exec(command: string | string[], options: ExecOptions = {}): Promise<ExecResult> {
    const cmd = typeof command === "string" ? command : command.map(shellQuote).join(" ");
    return this.#call("POST", this.#path("/exec"), { cmd, ...options });
  }

  /**
   * Makes sandboxes that start from a copy of this one's whole disk. A running
   * sandbox is saved first, in seconds, and keeps running. Forks that failed
   * to start come back in state "error"; resume() tries again.
   */
  async fork(options: ForkOptions = {}): Promise<Sandbox[]> {
    const forks = await this.#call<SandboxInfo[]>("POST", this.#path("/fork"), options);
    return forks.map((info) => new Sandbox(info, this.#call));
  }

  /** An HTTPS link to a port, on the deployment's preview domain. Anyone with it can use it. */
  async host(port: number): Promise<string> {
    const { url } = await this.#call<{ url: string }>("POST", this.#path("/hosts"), { port });
    return url;
  }

  /**
   * Starts the desktop (Chrome and a terminal), or resizes a running one, and
   * returns a link to its viewer on the preview domain. The first start
   * installs it, which takes a minute or two.
   */
  async desktop(options: { size?: string } = {}): Promise<Desktop & { url: string }> {
    const d = await this.#call<Desktop>("POST", this.#path("/desktop"), options);
    return { ...d, url: `${await this.host(d.port)}/${d.path}` };
  }

  async stopDesktop(): Promise<void> {
    await this.#call("DELETE", this.#path("/desktop"));
  }

  #path(suffix: string) {
    return sandboxPath(this.#info.id) + suffix;
  }

  async #update(method: string, suffix: string, body?: unknown): Promise<this> {
    this.#info = await this.#call<SandboxInfo>(method, this.#path(suffix), body);
    return this;
  }
}

const sandboxPath = (ref: string) => `/v1/sandboxes/${encodeURIComponent(ref)}`;

/** Quotes one argument for bash. */
export function shellQuote(arg: string): string {
  return /^[\w@%+=:,./-]+$/.test(arg) ? arg : `'${arg.replaceAll("'", `'\\''`)}'`;
}

// The CLI keeps its deployment in ~/.config/cloudex/config.json. Only Node-like
// runtimes have a file system; the module name is a variable so that bundlers
// for browsers and Workers leave it alone.
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
