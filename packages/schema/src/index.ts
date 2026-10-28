// Models shared by the API Worker, the CLI and the SDKs. Keep this the single
// source of truth: the OpenAPI spec, and from it the SDKs, are generated here.
import { z } from "zod";

// Container instance types on Cloudflare. standard-4 is 4 vCPU, 12 GiB and 20 GB of disk.
export const SandboxSize = z.enum(["lite", "standard-1", "standard-2", "standard-3", "standard-4"]);
export type SandboxSize = z.infer<typeof SandboxSize>;

export const SandboxState = z.enum(["creating", "starting", "running", "stopping", "stopped", "error"]);
export type SandboxState = z.infer<typeof SandboxState>;

export const SandboxStatus = z.object({
  id: z.string(),
  state: SandboxState,
});
export type SandboxStatus = z.infer<typeof SandboxStatus>;

export const Health = z.object({
  ok: z.literal(true),
  version: z.string(),
});
export type Health = z.infer<typeof Health>;
