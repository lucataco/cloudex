// cloudex-api: the /v1 control plane. Phase 1 authenticates with a single
// admin key held as a Worker secret; scoped keys and device login come later.
import { Hono } from "hono";
import type { Health } from "@cloudex/schema";

export { Sandbox } from "./sandbox";

const VERSION = "0.0.0";

const app = new Hono<{ Bindings: Env }>();

app.get("/v1/health", (c) => c.json({ ok: true, version: VERSION } satisfies Health));

app.use("/v1/*", async (c, next) => {
  const key = c.env.ADMIN_KEY;
  if (!key || !(await sameSecret(c.req.header("authorization") ?? "", `Bearer ${key}`))) {
    return c.json({ error: "unauthorized" }, 401);
  }
  await next();
});

app.get("/v1/sandboxes/:id", async (c) => {
  const id = c.req.param("id");
  return c.json(await c.env.SANDBOX.getByName(id).status(id));
});

export default app;

// Compares digests so the check takes the same time whatever the input.
async function sameSecret(given: string, expected: string): Promise<boolean> {
  const enc = new TextEncoder();
  const a = await crypto.subtle.digest("SHA-256", enc.encode(given));
  const b = await crypto.subtle.digest("SHA-256", enc.encode(expected));
  return crypto.subtle.timingSafeEqual(a, b);
}
