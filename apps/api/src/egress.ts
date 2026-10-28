// Outbound handler for guests. Requests a sandbox makes to http://cloudex.internal
// land here, in the Workers runtime, with the sandbox's id as a prop.
//   POST /save-on-term  the entrypoint's SIGTERM hook asks for a final save
//   POST /activity      a long-running job (an agent prompt) says it's still busy
import { WorkerEntrypoint } from "cloudflare:workers";

export class Egress extends WorkerEntrypoint<Env, { id: string }> {
  async fetch(req: Request): Promise<Response> {
    const url = new URL(req.url);
    if (url.hostname !== "cloudex.internal" || req.method !== "POST") {
      return new Response("not found", { status: 404 });
    }
    const sandbox = this.env.SANDBOX.getByName(this.ctx.props.id);
    switch (url.pathname) {
      case "/save-on-term":
        return Response.json(await sandbox.guestTerm());
      case "/activity":
        await sandbox.ping();
        return new Response("ok\n");
    }
    return new Response("not found", { status: 404 });
  }
}
