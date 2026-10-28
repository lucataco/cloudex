// One Durable Object per sandbox. It owns the container's whole lifecycle;
// D1 is only the index. Lifecycle (start, exec, snapshot, stop, TTL) lands in
// Phase 1, following the rules the spikes found:
// - setInactivityTimeout only after the box is ready, and set it to the TTL;
//   alarms and guest pings don't extend it, so the DO ends boxes itself.
// - Snapshot before destroy(); snapshots only work on a running box.
// - Every container call gets a timeout: start() hangs while a new image propagates.
import { DurableObject } from "cloudflare:workers";
import type { SandboxStatus } from "@cloudex/schema";

export class Sandbox extends DurableObject<Env> {
  async status(id: string): Promise<SandboxStatus> {
    return { id, state: this.ctx.container?.running ? "running" : "stopped" };
  }
}
