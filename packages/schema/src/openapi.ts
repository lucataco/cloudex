// The /v1 API as an OpenAPI 3.1 document, built from the models in index.ts.
// The API serves it at /v1/openapi.json, so clients for other languages can be
// generated from a running deployment.
import { z } from "zod";
import {
  ApiError,
  CreateSandbox,
  Desktop,
  DesktopRequest,
  ExecRequest,
  ExecResult,
  ForkRequest,
  Health,
  HostRequest,
  HostResult,
  ResumeRequest,
  Sandbox,
} from "./index.ts";

// Request bodies are described as sent (defaults make fields optional);
// responses as returned.
const requests = { CreateSandbox, ForkRequest, ResumeRequest, ExecRequest, HostRequest, DesktopRequest };
const responses = { Sandbox, ExecResult, HostResult, Desktop, Health, Error: ApiError };

function jsonSchema(model: z.ZodType, io: "input" | "output") {
  const { $schema: _, ...schema } = z.toJSONSchema(model, { io });
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

function op(operationId: string, summary: string, rest: object) {
  return { operationId, summary, ...rest, responses: { ...(rest as { responses?: object }).responses, ...errors } };
}

export function openapi(version: string) {
  const schemas: Record<string, object> = {};
  for (const [name, model] of Object.entries(requests)) schemas[name] = jsonSchema(model, "input");
  for (const [name, model] of Object.entries(responses)) schemas[name] = jsonSchema(model, "output");
  const sandbox = ok("The sandbox", ref("Sandbox"));

  return {
    openapi: "3.1.0",
    info: {
      title: "Cloudex API",
      version,
      description:
        "Agent sandboxes in your own Cloudflare account. `cloudex deploy` sets up this API " +
        "and its admin key; `cloudex status --show-key` prints both.",
      license: { name: "MIT", identifier: "MIT" },
    },
    servers: [{ url: "/" }],
    security: [{ bearer: [] }],
    components: {
      schemas,
      securitySchemes: { bearer: { type: "http", scheme: "bearer", description: "The deployment's admin key" } },
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
      "/v1/sandboxes/{ref}/exec": {
        parameters: [sandboxRef],
        post: op("exec", "Run a command with bash -lc and wait for it", {
          description: "Output is cut at 1 MiB per stream. Counts as activity for idle stop.",
          requestBody: body("ExecRequest"),
          responses: { 200: ok("How the command ended", ref("ExecResult")) },
        }),
      },
      "/v1/sandboxes/{ref}/hosts": {
        parameters: [sandboxRef],
        post: op("host", "Get an HTTPS link to a port, on the deployment's preview domain", {
          requestBody: body("HostRequest"),
          responses: { 200: ok("The link", ref("HostResult")) },
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
          description: "Binary messages each way. cloudex ssh uses port 22.",
          responses: { 101: { description: "Switching to the WebSocket" } },
        }),
      },
    },
  };
}
