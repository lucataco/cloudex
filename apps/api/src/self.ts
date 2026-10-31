// The API a sandbox gets for itself at http://cloudex.internal/v1, without a
// key: the outbound handler (egress.ts) knows which sandbox each request comes
// from, and these routes act on that sandbox only, as "self" or its id. The
// cloudex command inside sandboxes (guest.ts) uses them.
import { Hono, type Context } from "hono";
import { HostRequest, UpdateSandbox } from "@cloudex/schema";
import { hostLink, hostLinks, newMethod, onError, portParam, revokeLinks } from "./http";
import { fail } from "./util";

type Ctx = { Bindings: { env: Env; id: string } };

export const selfApi = new Hono<Ctx>();
selfApi.onError(onError);

function own(c: Context<Ctx>) {
  const { env, id } = c.env;
  const ref = c.req.param("ref");
  if (ref !== "self" && ref !== id) fail(403, "from inside a sandbox, the API manages only that sandbox, as self");
  return { id, stub: env.SANDBOX.getByName(id) };
}

selfApi.get("/v1/sandboxes/:ref", async (c) => c.json(await own(c).stub.get()));

selfApi.patch("/v1/sandboxes/:ref", async (c) => {
  const change = UpdateSandbox.parse(await c.req.json());
  const { id, stub } = own(c);
  return c.json(await newMethod(id, stub.set(change)));
});

// Answers at once; the sandbox saves and stops a moment later.
selfApi.post("/v1/sandboxes/:ref/stop", async (c) => {
  const { id, stub } = own(c);
  return c.json(await newMethod(id, stub.requestStop()), 202);
});

selfApi.post("/v1/sandboxes/:ref/hosts", async (c) => {
  const { port } = HostRequest.parse(await c.req.json());
  const { id, stub } = own(c);
  return c.json(await hostLink(c.env.env, id, stub, port));
});

selfApi.get("/v1/sandboxes/:ref/hosts", async (c) => {
  const { id, stub } = own(c);
  return c.json(await hostLinks(c.env.env, id, stub));
});

selfApi.delete("/v1/sandboxes/:ref/hosts", async (c) => {
  const { id, stub } = own(c);
  return c.json(await revokeLinks(id, stub));
});

selfApi.delete("/v1/sandboxes/:ref/hosts/:port", async (c) => {
  const port = portParam(c.req.param("port"));
  const { id, stub } = own(c);
  return c.json(await revokeLinks(id, stub, port));
});

selfApi.all("*", (c) =>
  c.json({ error: "not found: from inside a sandbox, the API can show, rename, set the idle time of, stop and host ports of that sandbox only" }, 404),
);
