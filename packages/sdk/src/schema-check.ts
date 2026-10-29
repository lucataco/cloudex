// Fails the typecheck when the SDK's types drift from the API's models. Not
// part of the published package.
import type * as schema from "@cloudex/schema";
import type {
  CreateOptions,
  Desktop,
  ExecOptions,
  ExecResult,
  ForkOptions,
  Health,
  Provider,
  ProviderKey,
  SandboxInfo,
} from "./index.ts";

// Each assignable to the other, with the same field names (assignability alone
// lets an optional field be missing on one side).
type Same<A, B> = [A] extends [B]
  ? [B] extends [A]
    ? [keyof A] extends [keyof B]
      ? [keyof B] extends [keyof A]
        ? true
        : false
      : false
    : false
  : false;

export const checks: [
  Same<SandboxInfo, schema.Sandbox>,
  Same<CreateOptions, schema.CreateSandbox>,
  Same<ForkOptions, schema.ForkRequest>,
  Same<ExecOptions & { cmd: string }, schema.ExecRequest>,
  Same<ExecResult, schema.ExecResult>,
  Same<Desktop, schema.Desktop>,
  Same<{ size?: string }, schema.DesktopRequest>,
  Same<Health, schema.Health>,
  Same<Provider, schema.Provider>,
  Same<ProviderKey, schema.ProviderKey>,
  Same<{ key: string }, schema.SetProviderKey>,
] = [true, true, true, true, true, true, true, true, true, true, true];
