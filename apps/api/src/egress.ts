// Outbound handler for guests. Requests a sandbox makes to http://cloudex.internal
// land here, in the Workers runtime, with the sandbox's id as a prop.
//   POST /save-on-term          the entrypoint's SIGTERM hook asks for a final save
//   POST /activity              a long-running job (an agent prompt) says it's still busy
//   PUT  /backup/<gen>/<n>      one part of a backup, written to R2 (backup.ts)
//   GET  /backup/<gen>/<n>      one part read back, for a restore
// The id comes from the props, so a guest can only reach its own backups.
//
// So do HTTPS requests to AI providers whose keys the deployment holds
// (keys.ts): the real key goes in here, on the way out.
import { WorkerEntrypoint } from "cloudflare:workers";
import { backupBucket, partKey } from "./backup";
import { PROVIDERS, providerForHost, providerKey } from "./keys";

// When each sandbox last counted a provider call as activity, per isolate.
const lastPing = new Map<string, number>();

const BACKUP_PART = /^\/backup\/(\d{1,15})\/(\d{1,6})$/;

export class Egress extends WorkerEntrypoint<Env, { id: string }> {
  async fetch(req: Request): Promise<Response> {
    const url = new URL(req.url);
    const provider = providerForHost(url.hostname);
    if (provider) return this.provider(req, url, provider);
    if (url.hostname !== "cloudex.internal") return new Response("not found", { status: 404 });
    const id = this.ctx.props.id;

    const part = BACKUP_PART.exec(url.pathname);
    if (part) return this.backupPart(req, partKey(id, part[1]!, part[2]!));

    if (req.method !== "POST") return new Response("not found", { status: 404 });
    const sandbox = this.env.SANDBOX.getByName(id);
    switch (url.pathname) {
      case "/save-on-term":
        return Response.json(await sandbox.guestTerm());
      case "/activity":
        await sandbox.ping();
        return new Response("ok\n");
    }
    return new Response("not found", { status: 404 });
  }

  private async provider(req: Request, url: URL, name: keyof typeof PROVIDERS): Promise<Response> {
    // Codex tries a WebSocket first and falls back to HTTPS on a refusal.
    if (req.headers.get("upgrade")?.toLowerCase() === "websocket") {
      return new Response("Cloudex passes provider calls over HTTPS only\n", { status: 426 });
    }
    const spec = PROVIDERS[name];
    const headers = new Headers(req.headers);
    const sent = spec.sent(headers);
    if (!sent || sent === spec.placeholder) {
      const key = await providerKey(this.env, name);
      if (key) spec.set(headers, key);
    }
    // An agent working counts as activity, at most twice a minute.
    const id = this.ctx.props.id;
    if (Date.now() - (lastPing.get(id) ?? 0) > 30_000) {
      lastPing.set(id, Date.now());
      this.ctx.waitUntil(this.env.SANDBOX.getByName(id).ping().catch(() => {}));
    }
    headers.delete("host");
    return fetch(url.toString(), { method: req.method, headers, body: req.body, redirect: "manual" });
  }

  private async backupPart(req: Request, key: string): Promise<Response> {
    const bucket = backupBucket(this.env);
    if (!bucket) return new Response("backups are off: this deployment has no R2 bucket\n", { status: 501 });
    if (req.method === "GET") {
      const obj = await bucket.get(key);
      return obj ? new Response(obj.body) : new Response("no such part\n", { status: 404 });
    }
    if (req.method !== "PUT" || !req.body) return new Response("expected PUT with a body\n", { status: 405 });
    const len = Number(req.headers.get("content-length"));
    if (!Number.isSafeInteger(len) || len <= 0) return new Response("content-length required\n", { status: 411 });
    // R2 needs the length up front for a streamed body.
    const fixed = new FixedLengthStream(len);
    const [, put] = await Promise.all([req.body.pipeTo(fixed.writable), bucket.put(key, fixed.readable)]);
    return new Response(`ok ${put?.size ?? len}\n`);
  }
}
