// Fails the typecheck when the SDK's types drift from the API's models. Not
// part of the published package.
import type * as schema from "@cloudex/schema";
import type {
  AgentEvent,
  AgentEvents,
  AgentInfo,
  CreateOptions,
  Desktop,
  ExecOptions,
  ExecResult,
  ForkOptions,
  Health,
  HostLink,
  Provider,
  PromptOptions,
  ProviderKey,
  RepoOptions,
  ResumeOptions,
  RevokedLinks,
  SandboxInfo,
  SandboxSettings,
  SettingsChange,
  UpdateOptions,
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
  Same<PromptOptions & { prompt: string }, schema.StartAgent>,
  Same<{ prompt: string; interrupt?: boolean }, schema.AgentMessage>,
  Same<AgentInfo, schema.Agent>,
  Same<AgentEvent, schema.AgentEvent>,
  Same<AgentEvents, schema.AgentEvents>,
  Same<RepoOptions, schema.Repo>,
  Same<SandboxSettings, schema.SandboxSettings>,
  Same<SettingsChange, schema.UpdateSettings>,
  Same<UpdateOptions, schema.UpdateSandbox>,
  Same<ResumeOptions, schema.ResumeRequest>,
  Same<HostLink, schema.HostLink>,
  Same<RevokedLinks, schema.RevokedLinks>,
] = [
  true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true,
  true, true,
];
