// Fails the typecheck when the SDK's types drift from the API's models. Not
// part of the published package.
import type * as schema from "@cloudex/schema";
import type { CreateOptions, Desktop, ExecOptions, ExecResult, ForkOptions, Health, SandboxInfo } from "./index.ts";

type Same<A, B> = [A] extends [B] ? ([B] extends [A] ? true : false) : false;

export const checks: [
  Same<SandboxInfo, schema.Sandbox>,
  Same<CreateOptions, schema.CreateSandbox>,
  Same<ForkOptions, schema.ForkRequest>,
  Same<ExecOptions & { cmd: string }, schema.ExecRequest>,
  Same<ExecResult, schema.ExecResult>,
  Same<Desktop, schema.Desktop>,
  Same<{ size?: string }, schema.DesktopRequest>,
  Same<Health, schema.Health>,
] = [true, true, true, true, true, true, true, true];
