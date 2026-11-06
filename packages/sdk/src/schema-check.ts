// Fails the typecheck when the SDK's types drift from the API's models. Not
// part of the published package.
import type * as schema from "@cloudex/schema";
import type {
  AgentEvent,
  AgentEvents,
  AgentInfo,
  ArchiveResult,
  Command,
  CommandOptions,
  CommandOutput,
  CommandSignal,
  CommandState,
  CreateOptions,
  Desktop,
  ExecEvent,
  ExecOptions,
  ExecResult,
  FileEntry,
  FileInfo,
  FileType,
  ForkOptions,
  Health,
  HostLink,
  Me,
  NewToken,
  Provider,
  PromptOptions,
  ProviderKey,
  RepoOptions,
  ResumeOptions,
  RevokedLinks,
  Role,
  SandboxInfo,
  SandboxSettings,
  SaveInfo,
  SettingsChange,
  Token,
  TokenOptions,
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
  Same<ExecOptions & { cmd: string; stream?: boolean }, schema.ExecRequest>,
  Same<ExecResult, schema.ExecResult>,
  Same<ExecEvent, schema.ExecEvent>,
  Same<CommandOptions & { cmd: string }, schema.StartCommand>,
  Same<CommandState, schema.CommandState>,
  Same<Command, schema.Command>,
  Same<CommandOutput, schema.CommandOutput>,
  Same<{ signal?: CommandSignal }, schema.KillCommand>,
  Same<FileType, schema.FileType>,
  Same<FileEntry, schema.FileEntry>,
  Same<FileInfo, schema.FileInfo>,
  Same<ArchiveResult, schema.ArchiveResult>,
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
  Same<SaveInfo, schema.Save>,
  Same<{ name: string }, schema.CreateSave>,
  Same<Role, schema.Role>,
  Same<Token, schema.Token>,
  Same<NewToken, schema.NewToken>,
  Same<TokenOptions, schema.CreateToken>,
  Same<Me, schema.Me>,
] = [
  true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true,
  true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true, true,
];
