// The /v1 API as an OpenAPI 3.1 document, built from the models in index.ts.
// The API serves it at /v1/openapi.json, so clients for other languages can be
// generated from a running deployment.
import { z } from "zod";
import {
  Agent,
  AgentEvents,
  AgentMessage,
  ApiError,
  ArchiveResult,
  Command,
  CommandOutput,
  CreateSandbox,
  CreateSave,
  CreateToken,
  Desktop,
  DeviceApprove,
  DeviceCode,
  DeviceDeny,
  DevicePoll,
  DeviceRequest,
  DeviceStart,
  DeviceToken,
  DesktopRequest,
  ExecEvent,
  ExecRequest,
  ExecResult,
  FileInfo,
  ForkRequest,
  Health,
  HostLink,
  HostRequest,
  HostResult,
  KillCommand,
  LoginLink,
  CreateLoginLink,
  StartSession,
  Me,
  NewToken,
  Provider,
  ProviderKey,
  ResumeRequest,
  RevokedLinks,
  Sandbox,
  SandboxSettings,
  Save,
  SetProviderKey,
  StartAgent,
  StartCommand,
  Token,
  UpdateSandbox,
  UpdateSettings,
} from "./index.ts";
import { CreateNode, NewNode, Node, NodeJoin, NodeJoined } from "./node.ts";

// Request bodies are described as sent (defaults make fields optional);
// responses as returned.
const requests = {
  CreateSandbox,
  CreateSave,
  ForkRequest,
  ResumeRequest,
  ExecRequest,
  StartCommand,
  KillCommand,
  HostRequest,
  DesktopRequest,
  SetProviderKey,
  StartAgent,
  AgentMessage,
  UpdateSandbox,
  UpdateSettings,
  CreateToken,
  DeviceStart,
  DevicePoll,
  DeviceApprove,
  DeviceDeny,
  CreateLoginLink,
  StartSession,
  CreateNode,
  NodeJoin,
};
const responses = {
  Sandbox,
  SandboxSettings,
  Save,
  ExecResult,
  ExecEvent,
  Command,
  CommandOutput,
  FileInfo,
  ArchiveResult,
  HostResult,
  HostLink,
  RevokedLinks,
  Desktop,
  Health,
  ProviderKey,
  Agent,
  AgentEvents,
  Me,
  Token,
  NewToken,
  DeviceCode,
  DeviceToken,
  DeviceRequest,
  LoginLink,
  Node,
  NewNode,
  NodeJoined,
  Error: ApiError,
};

// Responses stay open to fields added later, which a strict generated client
// would otherwise reject.
function jsonSchema(model: z.ZodType, io: "input" | "output") {
  const { $schema: _, ...schema } = z.toJSONSchema(model, {
    io,
    override: ({ jsonSchema }) => {
      if (io === "output" && jsonSchema.additionalProperties === false) delete jsonSchema.additionalProperties;
    },
  });
  return schema;
}

const ref = (name: keyof typeof requests | keyof typeof responses) => ({ $ref: `#/components/schemas/${name}` });
const json = (schema: object) => ({ content: { "application/json": { schema } } });
const body = (name: keyof typeof requests, required = true) => ({ required, ...json(ref(name)) });
const ok = (description: string, schema: object) => ({ description, ...json(schema) });
const errors = { default: ok("An error, with a message for people", ref("Error")) };

const sandboxRef = {
  name: "ref",
  in: "path",
  required: true,
  description: "The sandbox's id (cx-xxxxxx) or name",
  schema: { type: "string" },
};

const saveName = {
  name: "name",
  in: "path",
  required: true,
  description: "The save's name",
  schema: { type: "string" },
};

const agentRef = {
  name: "agent",
  in: "path",
  required: true,
  description: "The agent's id (ag-xxxxxxxx)",
  schema: { type: "string" },
};

const commandRef = {
  name: "cmd",
  in: "path",
  required: true,
  description: "The command's id (cm-xxxxxxxx)",
  schema: { type: "string" },
};

const pathParam = {
  name: "path",
  in: "query",
  required: true,
  description: "The file's path; a relative one starts in the user's home directory",
  schema: { type: "string" },
};

const rootParam = {
  name: "root",
  in: "query",
  description: "Act as root rather than the cloudex user",
  schema: { type: "boolean", default: false },
};

function op(operationId: string, summary: string, rest: object) {
  return { operationId, summary, ...rest, responses: { ...(rest as { responses?: object }).responses, ...errors } };
}

export function openapi(version: string) {
  const schemas: Record<string, object> = {};
  for (const [name, model] of Object.entries(requests)) schemas[name] = jsonSchema(model, "input");
  for (const [name, model] of Object.entries(responses)) schemas[name] = jsonSchema(model, "output");
  const sandbox = ok("The sandbox", ref("Sandbox"));
  const agent = ok("The agent", ref("Agent"));

  return {
    openapi: "3.1.0",
    info: {
      title: "Cloudex API",
      version,
      description:
        "Agent sandboxes in your own Cloudflare account. `cloudex deploy` sets up this API " +
        "and its admin key; `cloudex status --show-key` prints both. Access tokens (`cloudex tokens`) " +
        "work in place of the admin key, limited by their role.",
      license: { name: "MIT", identifier: "MIT" },
    },
    servers: [{ url: "/" }],
    security: [{ bearer: [] }],
    components: {
      schemas,
      securitySchemes: {
        bearer: { type: "http", scheme: "bearer", description: "The deployment's admin key, or an access token (cx_...)" },
      },
    },
    paths: {
      "/v1/health": {
        get: op("health", "Check the deployment; needs no key", {
          security: [],
          responses: { 200: ok("The deployment's version and features", ref("Health")) },
        }),
      },
      "/v1/openapi.json": {
        get: op("openapi", "This document; needs no key", {
          security: [],
          responses: { 200: { description: "The OpenAPI document" } },
        }),
      },
      "/v1/sandboxes": {
        get: op("listSandboxes", "List sandboxes, newest first", {
          responses: { 200: ok("The sandboxes", { type: "array", items: ref("Sandbox") }) },
        }),
        post: op("createSandbox", "Create and start a sandbox; answers once it's running", {
          requestBody: body("CreateSandbox", false),
          responses: { 201: sandbox },
        }),
      },
      "/v1/sandboxes/{ref}": {
        parameters: [sandboxRef],
        get: op("getSandbox", "Get a sandbox", { responses: { 200: sandbox } }),
        patch: op("updateSandbox", "Rename a sandbox, or change how long it may sit idle", {
          description: "A new idle time applies at once, counting from now.",
          requestBody: body("UpdateSandbox"),
          responses: { 200: sandbox },
        }),
        delete: op("deleteSandbox", "Delete a sandbox and its backups", {
          responses: {
            200: ok("Deleted", { type: "object", properties: { deleted: { type: "string" } }, required: ["deleted"] }),
          },
        }),
      },
      "/v1/sandboxes/{ref}/stop": {
        parameters: [sandboxRef],
        post: op("stopSandbox", "Save the disk, back up /home and stop", { responses: { 200: sandbox } }),
      },
      "/v1/sandboxes/{ref}/resume": {
        parameters: [sandboxRef],
        post: op("resumeSandbox", "Start a stopped sandbox from its save, or from its backup", {
          description: "With size, it runs on that size from then on.",
          requestBody: body("ResumeRequest", false),
          responses: { 200: sandbox },
        }),
      },
      "/v1/sandboxes/{ref}/fork": {
        parameters: [sandboxRef],
        post: op("forkSandbox", "Make sandboxes that start from a copy of this one's disk", {
          description:
            "A running sandbox is saved first and keeps running. Forks that fail to start are " +
            "returned in state error, with the reason; resuming one tries again.",
          requestBody: body("ForkRequest", false),
          responses: { 201: ok("The forks", { type: "array", items: ref("Sandbox") }) },
        }),
      },
      "/v1/sandboxes/{ref}/saves": {
        parameters: [sandboxRef],
        post: op("createSave", "Save a running sandbox's disk under a name, to start new sandboxes from", {
          description:
            "The save holds every file that differs from the sandbox image, and the sandbox's settings, in the " +
            "deployment's R2 bucket until it's deleted. The sandbox keeps running. A save of a taken name " +
            "replaces that save once it's done. Takes about 15 seconds per GB of changes.",
          requestBody: body("CreateSave"),
          responses: { 201: ok("The save", ref("Save")) },
        }),
      },
      "/v1/saves": {
        get: op("listSaves", "List the named saves, newest first", {
          responses: { 200: ok("The saves", { type: "array", items: ref("Save") }) },
        }),
      },
      "/v1/saves/{name}": {
        parameters: [saveName],
        get: op("getSave", "Get a named save", { responses: { 200: ok("The save", ref("Save")) } }),
        delete: op("deleteSave", "Delete a named save for good", {
          description: "Sandboxes already started from it keep their disks.",
          responses: {
            200: ok("Deleted", { type: "object", properties: { deleted: { type: "string" } }, required: ["deleted"] }),
          },
        }),
      },
      "/v1/saves/{name}/sandboxes": {
        parameters: [saveName],
        post: op("startFromSave", "Start sandboxes from a named save", {
          description:
            "Each restores the save onto a fresh sandbox before anything in it starts. Size and idle minutes " +
            "default to the saved sandbox's. Onto a newer sandbox image, only /home is restored, and the " +
            "sandbox's notice says so. Ones that fail to start are returned in state error, with the reason; " +
            "resuming one tries again.",
          requestBody: body("ForkRequest", false),
          responses: { 201: ok("The new sandboxes", { type: "array", items: ref("Sandbox") }) },
        }),
      },
      "/v1/sandboxes/{ref}/exec": {
        parameters: [sandboxRef],
        post: op("exec", "Run a command with bash -lc and wait for it", {
          description:
            "Output is cut at 1 MiB per stream. With stream, the answer is application/x-ndjson: one ExecEvent " +
            "per line as the output comes, uncut, ending with exit. A timeout stops the command and everything it " +
            "started; so does closing a stream. Counts as activity for idle stop.",
          requestBody: body("ExecRequest"),
          responses: {
            200: {
              description: "How the command ended, or with stream its output as it comes",
              content: {
                "application/json": { schema: ref("ExecResult") },
                "application/x-ndjson": { schema: ref("ExecEvent") },
              },
            },
          },
        }),
      },
      "/v1/sandboxes/{ref}/commands": {
        parameters: [sandboxRef],
        get: op("listCommands", "List the sandbox's background commands, newest first", {
          responses: { 200: ok("The commands", { type: "array", items: ref("Command") }) },
        }),
        post: op("startCommand", "Start a command in the background", {
          description:
            "Answers once it's running. Its output (standard output and error together) is kept on the sandbox's " +
            "disk; read it with the output call. While it writes output it counts as activity for idle stop. A stop " +
            "ends it.",
          requestBody: body("StartCommand"),
          responses: { 201: ok("The command", ref("Command")) },
        }),
      },
      "/v1/sandboxes/{ref}/commands/{cmd}": {
        parameters: [sandboxRef, commandRef],
        get: op("getCommand", "Get a background command", { responses: { 200: ok("The command", ref("Command")) } }),
        delete: op("forgetCommand", "Delete a finished command's record and output", {
          responses: { 200: ok("The command, as it was", ref("Command")) },
        }),
      },
      "/v1/sandboxes/{ref}/commands/{cmd}/output": {
        parameters: [
          sandboxRef,
          commandRef,
          {
            name: "after",
            in: "query",
            description: "Return the output from this byte offset; next in the answer is the one to pass next time",
            schema: { type: "integer", minimum: 0, default: 0 },
          },
          {
            name: "wait",
            in: "query",
            description: "Seconds to wait for output after `after`, or for the command to end, before answering",
            schema: { type: "integer", minimum: 0, maximum: 60, default: 0 },
          },
        ],
        get: op("commandOutput", "Read a command's output, waiting for more if asked", {
          description: "Returns up to 1 MiB at a time; ask again from next for the rest.",
          responses: { 200: ok("The output and the command", ref("CommandOutput")) },
        }),
      },
      "/v1/sandboxes/{ref}/commands/{cmd}/kill": {
        parameters: [sandboxRef, commandRef],
        post: op("killCommand", "Send a running command, and everything it started, a signal", {
          requestBody: body("KillCommand", false),
          responses: { 200: ok("The command", ref("Command")) },
        }),
      },
      "/v1/sandboxes/{ref}/files": {
        parameters: [sandboxRef, pathParam, rootParam],
        get: op("getFile", "A path's details and, for a directory, what's in it", {
          responses: { 200: ok("The path", ref("FileInfo")) },
        }),
        delete: op("deleteFile", "Delete a file, or with recursive a directory and everything in it", {
          parameters: [
            { name: "recursive", in: "query", description: "Delete a directory and everything in it", schema: { type: "boolean", default: false } },
          ],
          responses: { 200: ok("What was deleted", ref("FileInfo")) },
        }),
      },
      "/v1/sandboxes/{ref}/files/content": {
        parameters: [sandboxRef, pathParam, rootParam],
        get: op("readFile", "Download a file", {
          responses: {
            200: { description: "The file's contents", content: { "application/octet-stream": { schema: { type: "string", format: "binary" } } } },
          },
        }),
        put: op("writeFile", "Upload a file, making its directory if need be", {
          description:
            "Replaces the file whole, keeping its mode unless mode is given; writes through a symlink to its target. " +
            "Cloudflare limits a request body to 100 MB on most plans.",
          parameters: [{ name: "mode", in: "query", description: "Permission bits in octal, such as 755", schema: { type: "string" } }],
          requestBody: {
            required: true,
            content: { "application/octet-stream": { schema: { type: "string", format: "binary" } } },
          },
          responses: { 200: ok("The file", ref("FileInfo")) },
        }),
      },
      "/v1/sandboxes/{ref}/files/archive": {
        parameters: [sandboxRef, pathParam, rootParam],
        get: op("downloadArchive", "Download a directory's contents, or a file, as a tar.gz", {
          description: "Files the user can't read are left out.",
          responses: { 200: { description: "A tar.gz", content: { "application/gzip": { schema: { type: "string", format: "binary" } } } } },
        }),
        put: op("uploadArchive", "Unpack a tar.gz into a directory, making it if need be", {
          description:
            "The files are owned by the cloudex user, or by root with root. Cloudflare limits a request body to 100 MB on most plans.",
          requestBody: { required: true, content: { "application/gzip": { schema: { type: "string", format: "binary" } } } },
          responses: { 200: ok("Where it went and how many entries it held", ref("ArchiveResult")) },
        }),
      },
      "/v1/sandboxes/{ref}/hosts": {
        parameters: [sandboxRef],
        post: op("host", "Get an HTTPS link to a port, on the deployment's preview domain", {
          description: "The same port gives the same link until it's revoked.",
          requestBody: body("HostRequest"),
          responses: { 200: ok("The link", ref("HostResult")) },
        }),
        get: op("listHosts", "List the preview links handed out since v0.1.14 that still work", {
          description: "Links made before v0.1.14 still work until revoked but aren't listed; revoking every link ends them too.",
          responses: { 200: ok("The links", { type: "array", items: ref("HostLink") }) },
        }),
        delete: op("revokeHosts", "Revoke every preview link to the sandbox, the desktop's too", {
          description: "Including links handed out before they were listed. Asking for a port's link again gives a new one.",
          responses: { 200: ok("What was revoked", ref("RevokedLinks")) },
        }),
      },
      "/v1/sandboxes/{ref}/hosts/{port}": {
        parameters: [
          sandboxRef,
          { name: "port", in: "path", required: true, schema: { type: "integer", minimum: 1, maximum: 65535 } },
        ],
        delete: op("revokeHost", "Revoke a port's preview link", {
          description: "It stops working at once; asking for the port's link again gives a new one.",
          responses: { 200: ok("What was revoked", ref("RevokedLinks")) },
        }),
      },
      "/v1/sandboxes/{ref}/desktop": {
        parameters: [sandboxRef],
        post: op("startDesktop", "Start the desktop (installing it the first time) or resize it", {
          description: "The first start takes a minute or two. Open the viewer at <path> on <port>, through a host link.",
          requestBody: body("DesktopRequest", false),
          responses: { 200: ok("Where the desktop is served", ref("Desktop")) },
        }),
        delete: op("stopDesktop", "Stop the desktop and the programs on it", {
          responses: {
            200: ok("Stopped", { type: "object", properties: { stopped: { type: "boolean" } }, required: ["stopped"] }),
          },
        }),
      },
      "/v1/sandboxes/{ref}/settings": {
        parameters: [sandboxRef],
        get: op("getSettings", "The sandbox's settings: variable names, file paths and the repo", {
          description: "Values and file contents are never returned.",
          responses: { 200: ok("The settings", ref("SandboxSettings")) },
        }),
        patch: op("updateSettings", "Set or remove environment variables and files", {
          description:
            "null removes a variable or file. A running sandbox gets the change at once, in new shells; " +
            "a stopped one at its next start.",
          requestBody: body("UpdateSettings"),
          responses: { 200: ok("The settings", ref("SandboxSettings")) },
        }),
      },
      "/v1/sandboxes/{ref}/agents": {
        parameters: [sandboxRef],
        get: op("listAgents", "List the sandbox's agents, newest first", {
          responses: { 200: ok("The agents", { type: "array", items: ref("Agent") }) },
        }),
        post: op("startAgent", "Start Claude Code or Codex on a task", {
          description:
            "Runs the agent with no terminal, as the cloudex user in a login shell, so it uses provider keys. " +
            "Answers at once; follow it with the events. The agent and its conversation live on the sandbox's disk.",
          requestBody: body("StartAgent"),
          responses: { 201: agent },
        }),
      },
      "/v1/sandboxes/{ref}/agents/{agent}": {
        parameters: [sandboxRef, agentRef],
        get: op("getAgent", "Get an agent", { responses: { 200: agent } }),
      },
      "/v1/sandboxes/{ref}/agents/{agent}/events": {
        parameters: [
          sandboxRef,
          agentRef,
          {
            name: "after",
            in: "query",
            description: "Return the events after this seq; next in the answer is the one to pass next time",
            schema: { type: "integer", minimum: 0, default: 0 },
          },
          {
            name: "wait",
            in: "query",
            description: "Seconds to wait for an event while there's none after `after` and the agent is running",
            schema: { type: "integer", minimum: 0, maximum: 60, default: 0 },
          },
        ],
        get: op("agentEvents", "Read an agent's events, waiting for new ones if asked", {
          description: "Returns up to about 512 KiB of events at a time; ask again from next for the rest.",
          responses: { 200: ok("Events, and the agent as of now", ref("AgentEvents")) },
        }),
      },
      "/v1/sandboxes/{ref}/agents/{agent}/messages": {
        parameters: [sandboxRef, agentRef],
        post: op("sendAgentMessage", "Send an agent a message, which starts its next turn", {
          description:
            "The message continues the agent's conversation. While a turn runs it waits its turn, unless " +
            "interrupt is set, which stops the running turn and drops queued messages first.",
          requestBody: body("AgentMessage"),
          responses: { 200: agent },
        }),
      },
      "/v1/sandboxes/{ref}/agents/{agent}/interrupt": {
        parameters: [sandboxRef, agentRef],
        post: op("interruptAgent", "Stop an agent's running turn and drop its queued messages", {
          responses: { 200: agent },
        }),
      },
      "/v1/nodes": {
        post: op("createNode", "Add a node: your own Docker host that runs sandboxes (admins only)", {
          description:
            "Answers the node and a join token, which works once, for 30 minutes, and the command that installs " +
            "the agent with it. Adding a name that is still pending (never joined) or revoked gives it a new token; " +
            "a name in use is a 409.",
          requestBody: body("CreateNode"),
          responses: { 201: ok("The node, its join token and the install command", ref("NewNode")) },
        }),
        get: op("listNodes", "List the nodes, with whether each is online now", {
          responses: { 200: ok("The nodes", { type: "array", items: ref("Node") }) },
        }),
      },
      "/v1/nodes/{node}": {
        parameters: [{ name: "node", in: "path", required: true, description: "The node's name or id (nd-xxxxxxxx)", schema: { type: "string" } }],
        get: op("getNode", "Get a node; while it's online, with a round trip to its agent", {
          responses: { 200: ok("The node", ref("Node")) },
        }),
        delete: op("deleteNode", "Remove a node: its key stops working and its agent is disconnected (admins only)", {
          description:
            "The node stays listed as revoked; removing a revoked node forgets it. A node that still has sandboxes " +
            "is refused (409) unless force is set.",
          parameters: [{ name: "force", in: "query", description: "Remove it even though it has sandboxes", schema: { type: "boolean", default: false } }],
          responses: { 200: ok("Revoked or forgotten", { type: "object", properties: { revoked: { type: "string" }, deleted: { type: "string" } } }) },
        }),
      },
      "/v1/node/join": {
        post: op("joinNode", "The node agent's first call: trade a join token for its place, with its public key", {
          security: [],
          description:
            "For the cloudex-node agent, not for clients. Signed with the new key (see connectNode). The token works " +
            "once, for 30 minutes; one that is unknown, used or expired is a 401.",
          requestBody: body("NodeJoin"),
          responses: { 200: ok("The node it joined as", ref("NodeJoined")) },
        }),
      },
      "/v1/node/connect": {
        get: op("connectNode", "The node agent's control WebSocket", {
          security: [],
          description:
            "For the cloudex-node agent, not for clients. Headers: x-cx-node (its id), x-cx-ts (unix seconds), " +
            "x-cx-nonce (32 hex digits), x-cx-protocol, and x-cx-sig: Ed25519 in base64 over " +
            "METHOD\\nPATH?QUERY\\nSANDBOX\\nTS\\nNONCE, with SANDBOX empty. 60 s of clock skew are allowed, and a nonce " +
            "works once. A bad signature, a skewed clock, a reused nonce, or a removed or unknown node is a 401; " +
            "another protocol is a 426.",
          responses: { 101: { description: "Switching to the WebSocket" } },
        }),
      },
      "/v1/keys": {
        get: op("listKeys", "List the AI provider keys sandboxes use; the keys themselves are never returned", {
          responses: { 200: ok("The keys", { type: "array", items: ref("ProviderKey") }) },
        }),
      },
      "/v1/keys/{provider}": {
        parameters: [
          { name: "provider", in: "path", required: true, schema: { type: "string", enum: Provider.options } },
        ],
        put: op("setKey", "Set a provider's key", {
          description:
            "Sandboxes started from then on get a placeholder key, and the real one is added to their " +
            "HTTPS calls to the provider on the way out. A changed key reaches running sandboxes within a minute. " +
            "The key is tried with the provider first: a 400 says what the provider answered when it rejects the key.",
          requestBody: body("SetProviderKey"),
          responses: { 200: ok("The stored key", ref("ProviderKey")) },
        }),
        delete: op("deleteKey", "Remove a provider's key", {
          responses: {
            200: ok("Removed", { type: "object", properties: { deleted: { type: "string" } }, required: ["deleted"] }),
          },
        }),
      },
      "/v1/sandboxes/{ref}/tcp/{port}": {
        parameters: [
          sandboxRef,
          { name: "port", in: "path", required: true, schema: { type: "integer", minimum: 1, maximum: 65535 } },
          {
            name: "key",
            in: "query",
            description: "An SSH public key to add for the cloudex user first",
            schema: { type: "string" },
          },
        ],
        get: op("connectTcp", "A WebSocket carrying a TCP connection to a port in the sandbox", {
          description: "Binary messages each way. cloudex ssh uses port 22." +
            " A socket opened with a token or session closes with code 4401 once that token is revoked or expires, or the session signs out (checked about once a minute).",
          responses: { 101: { description: "Switching to the WebSocket" } },
        }),
      },
      "/v1/me": {
        get: op("getMe", "Who the caller is: the admin key, or a token with its role", {
          responses: { 200: ok("The caller", ref("Me")) },
        }),
      },
      "/v1/tokens": {
        post: op("createToken", "Make an access token (admins only)", {
          description:
            "Roles: admin (everything), user (sandboxes and saves, not tokens or provider keys) and read " +
            "(lists, info, agent events, command output, file reads). ownOnly limits it to the sandboxes it makes. " +
            "The answer holds the secret, which nothing shows again.",
          requestBody: body("CreateToken"),
          responses: { 201: ok("The token, with its secret", ref("NewToken")) },
        }),
        get: op("listTokens", "List the access tokens and dashboard sessions (admins only)", {
          responses: { 200: ok("The tokens", { type: "array", items: ref("Token") }) },
        }),
      },
      "/v1/tokens/{id}": {
        parameters: [{ name: "id", in: "path", required: true, description: "The token's id (tk-xxxxxxxx)", schema: { type: "string" } }],
        delete: op("revokeToken", "Revoke a token; it stops working at once (admins only)", {
          responses: {
            200: ok("Revoked", { type: "object", properties: { revoked: { type: "string" } }, required: ["revoked"] }),
          },
        }),
      },
      "/v1/auth/device": {
        post: op("startDeviceLogin", "Ask for a token for a computer without one (RFC 8628); needs no key", {
          security: [],
          description:
            "Show user_code to someone signed in, who approves it with cloudex tokens approve or in the dashboard, " +
            "then poll /v1/auth/token every interval seconds with device_code. Codes last 10 minutes. " +
            "429 when 20 are waiting, or 3 from the same address.",
          requestBody: body("DeviceStart"),
          responses: { 200: ok("The codes", ref("DeviceCode")) },
        }),
        get: op("listDeviceLogins", "List the device logins waiting for approval (admins only)", {
          responses: { 200: ok("The requests", { type: "array", items: ref("DeviceRequest") }) },
        }),
      },
      "/v1/auth/token": {
        post: op("pollDeviceLogin", "Collect a device login's token once it's approved; needs no key", {
          security: [],
          description:
            "Until then a 400 with error authorization_pending, or slow_down when asked sooner than the interval; " +
            "access_denied or expired_token end it. The token comes once.",
          requestBody: body("DevicePoll"),
          responses: { 200: ok("The token", ref("DeviceToken")) },
        }),
      },
      "/v1/auth/device/approve": {
        post: op("approveDeviceLogin", "Approve a device login by its user code (admins only)", {
          description:
            "Without a role, it gets user, or read if it asked for read: admin is granted only when named here, since anyone can ask for it. " +
            "The token expires in 90 days unless expiresInSec says otherwise; a dashboard session from it never lasts past that.",
          requestBody: body("DeviceApprove"),
          responses: { 200: ok("The approved request", ref("DeviceRequest")) },
        }),
      },
      "/v1/auth/device/deny": {
        post: op("denyDeviceLogin", "Refuse a device login by its user code (admins only)", {
          requestBody: body("DeviceDeny"),
          responses: { 200: ok("The refused request", ref("DeviceRequest")) },
        }),
      },
      "/v1/auth/links": {
        post: op("createLoginLink", "Make a one-time link that signs a browser in to the dashboard", {
          description:
            "Good for 60 seconds and once. The browser's session gets the caller's role, and a token's session " +
            "acts for that token. cloudex dashboard makes one and opens it. A dashboard session can't make one (403). " +
            "path must be one of the dashboard's pages (/, /saves, /access or /s/{sandbox}/...), else 400.",
          requestBody: body("CreateLoginLink", false),
          responses: { 201: ok("The link", ref("LoginLink")) },
        }),
      },
      "/v1/auth/session": {
        post: op("startSession", "Sign the dashboard in: trade a link code, a token or an approved device code for a session cookie", {
          security: [],
          description:
            "For the dashboard's own login page: it must come from the same origin, with the header X-Cloudex: 1. " +
            "Sets the __Host-cx_session cookie and answers who the session is. A device code that isn't approved yet " +
            "gets the same 400 errors as /v1/auth/token. A pasted dashboard session is refused (403).",
          requestBody: body("StartSession"),
          responses: { 200: ok("The session", ref("Me")) },
        }),
        delete: op("endSession", "Sign the dashboard out: revoke the cookie's session", {
          security: [],
          responses: { 200: ok("Signed out", { type: "object", properties: { signedOut: { type: "boolean" } }, required: ["signedOut"] }) },
        }),
      },
      "/v1/sandboxes/{ref}/terminal": {
        parameters: [
          sandboxRef,
          { name: "session", in: "query", description: "A tmux session to attach to, or make: reconnecting gets the same shell back", schema: { type: "string", pattern: "^[A-Za-z0-9_-]{1,40}$" } },
          { name: "root", in: "query", description: "A root shell rather than the cloudex user's", schema: { type: "boolean", default: false } },
          { name: "cols", in: "query", schema: { type: "integer", minimum: 2, maximum: 1000, default: 80 } },
          { name: "rows", in: "query", schema: { type: "integer", minimum: 2, maximum: 1000, default: 24 } },
        ],
        get: op("openTerminal", "A WebSocket to a login shell in the sandbox, under a pty", {
          description:
            "Binary messages are the terminal's bytes each way. Text messages are JSON: send {\"resize\":[cols,rows]}, " +
            "{\"ping\":1} (which counts as activity, so an open terminal keeps the sandbox up) or {\"kill\":1} " +
            "(ends a tmux session); the sandbox sends {\"exit\":code} when the shell ends." +
            " A socket opened with a token or session closes with code 4401 once that token is revoked or expires, or the session signs out (checked about once a minute).",
          responses: { 101: { description: "Switching to the WebSocket" } },
        }),
      },
      "/v1/sandboxes/{ref}/desktop/vnc": {
        parameters: [sandboxRef],
        get: op("connectDesktop", "A WebSocket carrying the desktop's VNC (RFB) connection", {
          description:
            "Start the desktop first (POST /v1/sandboxes/{ref}/desktop). The dashboard's noVNC speaks RFB over it." +
            " A socket opened with a token or session closes with code 4401 once that token is revoked or expires, or the session signs out (checked about once a minute).",
          responses: { 101: { description: "Switching to the WebSocket" } },
        }),
      },
    },
  };
}
