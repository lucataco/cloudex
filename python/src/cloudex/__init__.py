"""The Cloudex SDK: create, fork and drive sandboxes from Python.

It talks to the /v1 API that `cloudex deploy` set up in your Cloudflare
account, using only the standard library.

    from cloudex import Cloudex

    cx = Cloudex()  # the deployment saved by `cloudex login`, or $CLOUDEX_API_URL and $CLOUDEX_API_KEY
    box = cx.create(name="web")
    print(box.exec("uname -a").stdout)
    forks = box.fork(count=3, name="try")

    for event in box.exec_stream("npm test"):  # output as it comes
        print(event.data, end="")

    agent = box.prompt("make the tests pass")  # Claude Code, with no terminal
    for event in agent.events():
        print(event["type"])
    print(agent.last_message)
"""

from __future__ import annotations

import http.client
import json
import os
import re
import shlex
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import IO, Any, Dict, Iterator, List, Optional, Sequence, Union

__all__ = [
    "Agent",
    "ArchiveResult",
    "Cloudex",
    "CloudexError",
    "Command",
    "CommandOutput",
    "Desktop",
    "ExecEvent",
    "ExecResult",
    "FileEntry",
    "FileInfo",
    "Health",
    "HostLink",
    "ProviderKey",
    "Sandbox",
    "Save",
    "Settings",
]
__version__ = "0.1.17"


class CloudexError(Exception):
    """An error from the API, with its HTTP status (0 when there was no request)."""

    def __init__(self, status: int, message: str):
        super().__init__(message)
        self.status = status
        self.message = message


@dataclass(frozen=True)
class ExecResult:
    exit_code: int
    stdout: str  # each stream is cut at 1 MiB
    stderr: str
    timed_out: bool
    ms: int


@dataclass(frozen=True)
class ExecEvent:
    """Part of a streamed exec: output as it comes (type "stdout" or "stderr",
    in data), then "exit" with exit_code, timed_out and ms.

    Output is UTF-8 text; bytes that aren't become U+FFFD, so read binary
    files with read_file.
    """

    type: str
    data: str = ""
    exit_code: Optional[int] = None
    timed_out: Optional[bool] = None
    ms: Optional[int] = None


@dataclass(frozen=True)
class Command:
    """A background command. Times are milliseconds since the epoch.

    state is running; exited, with exit_code; killed by a signal, which
    includes a kill and a timeout (timed_out); or lost: the sandbox stopped,
    or was forked or saved, while it ran.
    """

    id: str  # cm-xxxxxxxx
    cmd: str
    root: bool
    cwd: str
    state: str
    exit_code: Optional[int]
    signal: Optional[str]  # the signal that ended it, such as TERM or KILL
    timed_out: bool
    timeout_sec: int  # 0: none
    started_at: int
    ended_at: Optional[int]
    output_bytes: int  # the size of its output so far


@dataclass(frozen=True)
class CommandOutput:
    """Part of a command's output (stdout and stderr together), from byte offset
    offset to next, as UTF-8 text. Pass next as after to read on."""

    data: str
    offset: int
    next: int
    command: Command  # the command as of this read


@dataclass(frozen=True)
class FileEntry:
    """One entry in a directory."""

    name: str
    type: str  # file, dir, symlink or other
    size: int
    mode: str  # permission bits in octal, such as 644
    mtime: int  # milliseconds since the epoch
    target: Optional[str] = None  # a symlink's target


@dataclass(frozen=True)
class FileInfo:
    """A path's details and, for a directory, what's in it."""

    path: str  # absolute
    type: str  # file, dir, symlink or other
    size: int
    mode: str  # permission bits in octal, such as 644
    mtime: int  # milliseconds since the epoch
    target: Optional[str] = None  # a symlink's target
    entries: Optional[List[FileEntry]] = None  # a directory's, sorted by name, at most 10,000
    truncated: bool = False  # the directory had more entries than these


@dataclass(frozen=True)
class ArchiveResult:
    """What an archive upload unpacked, and where."""

    path: str
    entries: int


@dataclass(frozen=True)
class Desktop:
    url: str  # the viewer, on the preview domain; anyone with it can use the desktop
    port: int
    path: str
    size: str
    installed: bool  # this call installed it (the first start takes a minute or two)
    started: bool  # this call started it, rather than finding it running


@dataclass(frozen=True)
class ProviderKey:
    provider: str  # anthropic or openai
    hint: str  # the key's last 4 characters; the key itself is never returned
    updated_at: int  # milliseconds since the epoch


@dataclass(frozen=True)
class Settings:
    """A sandbox's settings. Values and file contents never come back."""

    env: List[str]
    files: List[str]  # as ~/path
    repo: Optional[Dict[str, Any]]  # url, dir, branch


@dataclass(frozen=True)
class HostLink:
    """A preview link handed out that still works."""

    port: int
    url: str
    created_at: int  # milliseconds since the epoch


@dataclass(frozen=True)
class Save:
    """A named save: a copy of a sandbox's disk in the deployment's R2 bucket,
    kept until it's deleted, that new sandboxes start from
    (Cloudex.start_from_save). It holds every file that differs from the
    sandbox image, and the sandbox's settings. Onto a newer image, only /home
    is restored.
    """

    name: str
    id: str  # sv-xxxxxxxxxx, new each time the name is saved
    sandbox: str  # the id of the sandbox it was made from
    kind: str  # disk: every file that differs from the image; home: /home only, from an image before v0.1.15
    size: str  # the defaults for sandboxes started from it
    idle_minutes: int
    files: int  # paths it holds, and their size
    raw_bytes: int
    bytes: int  # its size in R2, compressed
    created_at: int  # milliseconds since the epoch
    ms: int  # how long making it took
    replaced: Optional[bool] = None  # only from Sandbox.save: whether it replaced an earlier save of the name


@dataclass(frozen=True)
class Health:
    ok: bool
    version: str
    preview_zone: Optional[str]
    backups: bool


def _drop_none(**fields: Any) -> Dict[str, Any]:
    return {k: v for k, v in fields.items() if v is not None}


_OLDER_DEPLOYMENT = "this deployment is older than the SDK and doesn't have that call; update it with: cloudex deploy"
_HTML_TITLE = re.compile(r"<title[^>]*>(.*?)</title>", re.IGNORECASE | re.DOTALL)


def _plain_error(status: int, body: str) -> str:
    # An answer that isn't the API's JSON, such as Cloudflare's HTML error page,
    # becomes its status and title; other text is cut at 500 characters.
    body = body.strip()
    if body.startswith("<"):
        m = _HTML_TITLE.search(body)
        title = " ".join(m.group(1).split()) if m else ""
        return title if title.startswith(str(status)) else f"{status} {title}".strip()
    return body[:500] + "\u2026" if len(body) > 500 else body


def _saved_deployment(config_dir: Optional[str]) -> Dict[str, Any]:
    # The CLI keeps its deployment in ~/.config/cloudex/config.json.
    path = Path(config_dir) if config_dir else Path.home() / ".config" / "cloudex"
    try:
        return json.loads((path / "config.json").read_text())
    except (OSError, ValueError):
        return {}


def _normalize_url(url: str) -> str:
    # As the CLI's NormalizeURL: no trailing slashes, https:// when there's no
    # scheme, the scheme and host in lowercase, and no :443 on https, so that
    # the saved key matches its deployment however either URL is written.
    u = url.strip().rstrip("/")
    if not u:
        return u
    if "://" not in u:
        u = "https://" + u
    scheme, rest = u.split("://", 1)
    host, slash, path = rest.partition("/")
    scheme, host = scheme.lower(), host.lower()
    if scheme == "https" and host.endswith(":443"):
        host = host[: -len(":443")]
    return f"{scheme}://{host}{slash}{path}"


class Cloudex:
    """A Cloudex deployment.

    The URL and key come from the arguments, then $CLOUDEX_API_URL and
    $CLOUDEX_API_KEY, then the deployment saved by `cloudex deploy` or
    `cloudex login`, the same order as the CLI. The saved key is used only
    with the saved URL: for any other URL, pass api_key or set
    $CLOUDEX_API_KEY. timeout is in seconds per
    request; by default there's none, since starting or forking can take
    minutes the first time on a host.
    """

    def __init__(self, api_url: Optional[str] = None, api_key: Optional[str] = None, timeout: Optional[float] = None):
        url = api_url or os.environ.get("CLOUDEX_API_URL")
        key = api_key or os.environ.get("CLOUDEX_API_KEY")
        if not url or not key:
            saved = _saved_deployment(os.environ.get("CLOUDEX_CONFIG_DIR"))
            url = url or saved.get("apiUrl")
            # The saved key goes only to the saved URL, so a URL passed in can't be sent the admin key.
            if not key and url and saved.get("apiUrl") and _normalize_url(url) == _normalize_url(saved["apiUrl"]):
                key = saved.get("apiKey")
        url = _normalize_url(url or "")
        if not url:
            raise CloudexError(
                0,
                "no Cloudex deployment: pass api_url and api_key, set CLOUDEX_API_URL and CLOUDEX_API_KEY, or run cloudex login",
            )
        if not key:
            raise CloudexError(
                0,
                f"no API key for {url}: pass api_key or set CLOUDEX_API_KEY. "
                "The CLI's saved key is used only with the deployment it was saved for",
            )
        self.api_url = url
        self._key = key
        self._timeout = timeout

    def __repr__(self) -> str:
        return f"Cloudex({self.api_url!r})"

    def health(self) -> Health:
        """The deployment's version and features."""
        h = self._call("GET", "/v1/health")
        return Health(ok=h["ok"], version=h["version"], preview_zone=h.get("previewZone"), backups=bool(h.get("backups")))

    def list(self) -> List["Sandbox"]:
        """All sandboxes, newest first, as a summary.

        get() or refresh() has every field: notice, forkedFrom, fromSave,
        providerKeys and the backup* ones.
        """
        return [Sandbox(self, info) for info in self._call("GET", "/v1/sandboxes")]

    def get(self, ref: str) -> "Sandbox":
        """A sandbox by id (cx-xxxxxx) or name."""
        return Sandbox(self, self._call("GET", _sandbox_path(ref)))

    def create(
        self,
        name: Optional[str] = None,
        size: Optional[str] = None,
        idle_minutes: Optional[int] = None,
        env: Optional[Dict[str, str]] = None,
        files: Optional[Dict[str, str]] = None,
        repo: Union[str, Dict[str, str], None] = None,
    ) -> "Sandbox":
        """Creates a sandbox and returns once it's running, usually in about 10 seconds.

        size is standard-1 to standard-4 (default standard-2). idle_minutes
        (default 30) is how long without activity before it saves and stops;
        0 waits for the 6-hour platform limit. env and files (paths in the home
        directory, such as ~/.npmrc) are applied at every start; repo, an
        https:// URL with #branch if wanted, or {"url", "dir", "branch"}, is
        cloned into the home directory. A failed clone leaves a notice.
        """
        body = _drop_none(name=name, size=size, idleMinutes=idle_minutes, env=env, files=files, repo=repo)
        return Sandbox(self, self._call("POST", "/v1/sandboxes", body))

    def list_keys(self) -> List[ProviderKey]:
        """The AI provider keys sandboxes use; the keys themselves are never returned."""
        return [_provider_key(k) for k in self._call("GET", "/v1/keys")]

    def set_key(self, provider: str, key: str) -> ProviderKey:
        """Sets a provider's key ("anthropic" or "openai").

        Sandboxes started from then on get a placeholder, and the real key is
        added to their HTTPS calls to the provider on the way out, so it never
        enters a sandbox, its saves, backups or forks. A changed key reaches
        running sandboxes within a minute. The key is tried with the provider
        first; if the provider rejects it, this raises a CloudexError (status
        400) with what the provider said.
        """
        return _provider_key(self._call("PUT", "/v1/keys/" + urllib.parse.quote(provider, safe=""), {"key": key}))

    def delete_key(self, provider: str) -> None:
        self._call("DELETE", "/v1/keys/" + urllib.parse.quote(provider, safe=""))

    def list_saves(self) -> List[Save]:
        """The named saves, newest first. Make one with Sandbox.save."""
        return [_save(s) for s in self._call("GET", "/v1/saves")]

    def get_save(self, name: str) -> Save:
        return _save(self._call("GET", _save_path(name)))

    def delete_save(self, name: str) -> None:
        """Deletes a named save for good. Sandboxes started from it keep their disks."""
        self._call("DELETE", _save_path(name))

    def start_from_save(
        self,
        save: str,
        count: int = 1,
        name: Optional[str] = None,
        size: Optional[str] = None,
        idle_minutes: Optional[int] = None,
    ) -> List["Sandbox"]:
        """Starts sandboxes (1 to 10) from a named save, with the same options as a fork.

        Size and idle minutes default to the saved sandbox's. Restoring takes
        about 15 seconds per GB. Ones that failed to start come back in state
        "error"; resume() tries again.
        """
        body = _drop_none(name=name, count=count, size=size, idleMinutes=idle_minutes)
        return [Sandbox(self, info) for info in self._call("POST", _save_path(save) + "/sandboxes", body)]

    def _call(self, method: str, path: str, body: Optional[Dict[str, Any]] = None) -> Any:
        data = None if body is None else json.dumps(body).encode()
        with self._open(method, path, data, None if data is None else "application/json") as res:
            text = self._read(res).decode()
        return json.loads(text) if text else None

    def _unreachable(self, e: Exception) -> CloudexError:
        reason = e.reason if isinstance(e, urllib.error.URLError) else e
        return CloudexError(0, f"can't reach {self.api_url}: {reason}")

    def _read(self, res: Any) -> bytes:
        try:
            return res.read()
        except (OSError, http.client.HTTPException) as e:
            raise self._unreachable(e) from None

    def _lines(self, res: Any) -> Iterator[bytes]:
        try:
            yield from res
        except (OSError, http.client.HTTPException) as e:
            raise self._unreachable(e) from None

    def _open(self, method: str, path: str, data: Any = None, content_type: Optional[str] = None) -> Any:
        # A request, answered with the response unread; an error status raises.
        try:
            req = urllib.request.Request(self.api_url + path, data=data, method=method)
        except ValueError as e:  # no scheme, say
            raise self._unreachable(e) from None
        req.add_header("Authorization", f"Bearer {self._key}")
        req.add_header("User-Agent", f"cloudex-python/{__version__}")
        if content_type:
            req.add_header("Content-Type", content_type)
        if data is not None and not isinstance(data, bytes):
            # A file, sent from where it is to its end: give the length, or urllib sends chunks.
            here = data.tell()
            req.add_header("Content-Length", str(data.seek(0, os.SEEK_END) - here))
            data.seek(here)
        try:
            return urllib.request.urlopen(req, timeout=self._timeout)
        except urllib.error.HTTPError as e:
            text = self._read(e).decode(errors="replace")
            try:
                message = json.loads(text)["error"]
            except (ValueError, KeyError, TypeError):
                message = _plain_error(e.code, text) or e.reason
            if e.code == 404 and message == "not found":
                message = _OLDER_DEPLOYMENT
            raise CloudexError(e.code, str(message)) from None
        except (OSError, http.client.HTTPException) as e:  # URLError, a timeout, a reset
            raise self._unreachable(e) from None


def _provider_key(k: Dict[str, Any]) -> ProviderKey:
    return ProviderKey(provider=k["provider"], hint=k["hint"], updated_at=k["updatedAt"])


def _settings(s: Dict[str, Any]) -> Settings:
    return Settings(env=s["env"], files=s["files"], repo=s.get("repo"))


def _save(s: Dict[str, Any]) -> Save:
    return Save(
        name=s["name"],
        id=s["id"],
        sandbox=s["sandbox"],
        kind=s["kind"],
        size=s["size"],
        idle_minutes=s["idleMinutes"],
        files=s["files"],
        raw_bytes=s["rawBytes"],
        bytes=s["bytes"],
        created_at=s["createdAt"],
        ms=s["ms"],
        replaced=s.get("replaced"),
    )


def _command(c: Dict[str, Any]) -> Command:
    return Command(
        id=c["id"],
        cmd=c["cmd"],
        root=c["root"],
        cwd=c["cwd"],
        state=c["state"],
        exit_code=c.get("exitCode"),
        signal=c.get("signal"),
        timed_out=c["timedOut"],
        timeout_sec=c["timeoutSec"],
        started_at=c["startedAt"],
        ended_at=c.get("endedAt"),
        output_bytes=c["outputBytes"],
    )


def _file_info(f: Dict[str, Any]) -> FileInfo:
    entries = f.get("entries")
    return FileInfo(
        path=f["path"],
        type=f["type"],
        size=f["size"],
        mode=f["mode"],
        mtime=f["mtime"],
        target=f.get("target"),
        entries=None if entries is None else [
            FileEntry(name=e["name"], type=e["type"], size=e["size"], mode=e["mode"], mtime=e["mtime"], target=e.get("target"))
            for e in entries
        ],
        truncated=bool(f.get("truncated")),
    )


def _upload(data: Union[bytes, bytearray, memoryview, str, IO[bytes]]) -> Any:
    # Bytes, or a binary file that can seek, which is streamed; anything else is read whole.
    if isinstance(data, str):
        return data.encode()
    if isinstance(data, (bytes, bytearray, memoryview)):
        return bytes(data)
    seekable = getattr(data, "seekable", None)
    return data if seekable and seekable() else data.read()


def _sandbox_path(ref: str) -> str:
    return "/v1/sandboxes/" + urllib.parse.quote(ref, safe="")


def _save_path(name: str) -> str:
    return "/v1/saves/" + urllib.parse.quote(name, safe="")


class Sandbox:
    """A sandbox. Its fields are as of the last call that returned it; refresh() updates them.

    info holds everything the API reports (camelCase keys, times in
    milliseconds since the epoch); id, name and state are the common ones.
    """

    def __init__(self, client: Cloudex, info: Dict[str, Any]):
        self._client = client
        self.info = info

    @property
    def id(self) -> str:
        return self.info["id"]

    @property
    def name(self) -> Optional[str]:
        return self.info.get("name")

    @property
    def state(self) -> str:
        return self.info["state"]

    @property
    def forked_from(self) -> Optional[str]:
        return self.info.get("forkedFrom")

    @property
    def from_save(self) -> Optional[str]:
        """The named save this sandbox started from."""
        return self.info.get("fromSave")

    @property
    def provider_keys(self) -> List[str]:
        """Providers whose keys this run uses without holding them (see Cloudex.set_key)."""
        return self.info.get("providerKeys") or []

    def __repr__(self) -> str:
        label = f" {self.name!r}" if self.name else ""
        return f"<Sandbox {self.id}{label} {self.state}>"

    def _path(self, suffix: str = "") -> str:
        return _sandbox_path(self.id) + suffix

    def _command_path(self, id: str) -> str:
        return self._path("/commands/" + urllib.parse.quote(id, safe=""))

    def _file_path(self, suffix: str, path: str, root: bool, recursive: bool = False, mode: Optional[str] = None) -> str:
        q = "?path=" + urllib.parse.quote(path, safe="/~")
        if root:
            q += "&root=true"
        if recursive:
            q += "&recursive=true"
        if mode:
            q += "&mode=" + urllib.parse.quote(mode, safe="")
        return self._path("/files" + suffix + q)

    def _update(self, method: str, suffix: str, body: Optional[Dict[str, Any]] = None) -> "Sandbox":
        self.info = self._client._call(method, self._path(suffix), body)
        return self

    def refresh(self) -> "Sandbox":
        return self._update("GET", "")

    def stop(self) -> "Sandbox":
        """Saves the disk, backs up /home and stops."""
        return self._update("POST", "/stop")

    def resume(self, from_backup: bool = False, size: Optional[str] = None) -> "Sandbox":
        """Starts from the save, or with from_backup, from a fresh image with /home restored.

        With size, it runs on that size from now on; the disk must fit the new size's.
        """
        body = _drop_none(fromBackup=True if from_backup else None, size=size)
        return self._update("POST", "/resume", body or None)

    def update(self, name: Optional[str] = None, idle_minutes: Optional[int] = None) -> "Sandbox":
        """Renames the sandbox, or changes how long it may sit idle; a new idle time applies at once."""
        return self._update("PATCH", "", _drop_none(name=name, idleMinutes=idle_minutes))

    def delete(self) -> None:
        """Deletes the sandbox and its backups."""
        self._client._call("DELETE", self._path())

    def exec(
        self,
        command: Union[str, Sequence[str]],
        root: bool = False,
        cwd: Optional[str] = None,
        timeout_sec: Optional[int] = None,
    ) -> ExecResult:
        """Runs a command with `bash -lc` and waits for it (default limit 600 s, at most 3600).

        A string is run as shell code; a list is quoted, one argument per
        element. cwd defaults to the home directory; a relative path, or one
        starting with ~, is in it. A failing command doesn't raise: check
        exit_code.
        """
        cmd = command if isinstance(command, str) else shlex.join(command)
        body = _drop_none(cmd=cmd, root=root or None, cwd=cwd, timeoutSec=timeout_sec)
        r = self._client._call("POST", self._path("/exec"), body)
        return ExecResult(exit_code=r["exitCode"], stdout=r["stdout"], stderr=r["stderr"], timed_out=r["timedOut"], ms=r["ms"])

    def exec_stream(
        self,
        command: Union[str, Sequence[str]],
        root: bool = False,
        cwd: Optional[str] = None,
        timeout_sec: Optional[int] = None,
    ) -> Iterator[ExecEvent]:
        """Runs a command like exec, yielding its output as it comes.

        Yields ExecEvents: "stdout" and "stderr" with data, then "exit" with
        exit_code. Streamed output isn't cut. Closing the generator early, or
        leaving its loop with break, stops the command. If the stream breaks
        off it raises CloudexError (status 0); the command may still be
        running. The API sends something every 15 seconds, so a client
        timeout must be longer than that. An API before v0.1.16 answers once
        the command ends, so all its output comes at once.
        """
        cmd = command if isinstance(command, str) else shlex.join(command)
        body = _drop_none(cmd=cmd, root=root or None, cwd=cwd, timeoutSec=timeout_sec)
        body["stream"] = True
        with self._client._open("POST", self._path("/exec"), json.dumps(body).encode(), "application/json") as res:
            if "ndjson" not in (res.headers.get("Content-Type") or ""):
                r = json.loads(self._client._read(res).decode())
                if r["stdout"]:
                    yield ExecEvent("stdout", r["stdout"])
                if r["stderr"]:
                    yield ExecEvent("stderr", r["stderr"])
                yield ExecEvent("exit", exit_code=r["exitCode"], timed_out=r["timedOut"], ms=r["ms"])
                return
            for line in self._client._lines(res):
                if not line.strip():
                    continue
                e = json.loads(line)
                if e["type"] in ("stdout", "stderr"):
                    yield ExecEvent(e["type"], e["data"])
                elif e["type"] == "exit":
                    yield ExecEvent("exit", exit_code=e["exitCode"], timed_out=e["timedOut"], ms=e["ms"])
                    return
                elif e["type"] == "error":
                    raise CloudexError(0, e["error"])
                # keepalive, and anything newer, is skipped
        raise CloudexError(0, "the exec stream ended before the command did")

    def start_command(
        self,
        command: Union[str, Sequence[str]],
        root: bool = False,
        cwd: Optional[str] = None,
        timeout_sec: Optional[int] = None,
    ) -> Command:
        """Starts a command in the background and returns once it's running.

        A string is run as shell code with `bash -lc`; a list is quoted.
        timeout_sec stops it after that long; by default it runs until it ends,
        is killed or the sandbox stops. Its output (stdout and stderr together)
        is kept on the sandbox's disk: read it with command_output or
        follow_command.
        """
        cmd = command if isinstance(command, str) else shlex.join(command)
        body = _drop_none(cmd=cmd, root=root or None, cwd=cwd, timeoutSec=timeout_sec)
        return _command(self._client._call("POST", self._path("/commands"), body))

    def list_commands(self) -> List[Command]:
        """The sandbox's background commands, newest first."""
        return [_command(c) for c in self._client._call("GET", self._path("/commands"))]

    def get_command(self, id: str) -> Command:
        """A background command by id (cm-xxxxxxxx)."""
        return _command(self._client._call("GET", self._command_path(id)))

    def command_output(self, id: str, after: int = 0, wait: int = 0) -> CommandOutput:
        """A command's output from byte offset after, up to 1 MiB.

        With wait (seconds, at most 60), waits for output after that or for the
        command to end. Pass next as after to read on.
        """
        o = self._client._call("GET", f"{self._command_path(id)}/output?after={after}&wait={wait}")
        return CommandOutput(data=o["data"], offset=o["offset"], next=o["next"], command=_command(o["command"]))

    def follow_command(self, id: str, after: int = 0) -> Iterator[CommandOutput]:
        """Follows a command's output from after until it ends and all of it is read.

        Each item is a piece of output; the last one's command tells how it
        ended. Each request waits up to 25 seconds, so a client timeout must
        be longer than that.
        """
        # Once the command has ended, read only what its log held then, so
        # that something it left running can't keep this going.
        end = -1
        while True:
            page = self.command_output(id, after=after, wait=25)
            c = page.command
            if c.state != "running" and end < 0:
                end = c.output_bytes
            ended = end >= 0 and (page.next >= end or page.next == after)
            if page.data or ended:
                yield page
            if ended:
                return
            after = page.next

    def kill_command(self, id: str, signal: Optional[str] = None) -> Command:
        """Sends a running command, and everything it started, a signal: TERM (the default), INT, HUP or KILL."""
        return _command(self._client._call("POST", self._command_path(id) + "/kill", _drop_none(signal=signal)))

    def forget_command(self, id: str) -> Command:
        """Deletes a finished command's record and output; a running one must be killed first."""
        return _command(self._client._call("DELETE", self._command_path(id)))

    def stat(self, path: str, root: bool = False) -> FileInfo:
        """A path's details and, for a directory, what's in it (entries).

        A relative path, or one starting with ~, is in the home directory.
        With root, acts as root rather than the cloudex user; so do the other
        file calls.
        """
        return _file_info(self._client._call("GET", self._file_path("", path, root)))

    def read_file(self, path: str, root: bool = False) -> bytes:
        """A file's contents."""
        with self._client._open("GET", self._file_path("/content", path, root)) as res:
            return self._client._read(res)

    def write_file(
        self,
        path: str,
        data: Union[bytes, str, IO[bytes]],
        mode: Union[str, int, None] = None,
        root: bool = False,
    ) -> FileInfo:
        """Writes a file whole, making its directory if need be.

        data is bytes, a str (sent as UTF-8) or a binary file. The file keeps
        its mode unless mode is given, in octal: "755" or 0o755. Cloudflare
        limits a request to 100 MB on most plans.
        """
        # The API wants 3 or 4 octal digits.
        mode_text = format(mode, "03o") if isinstance(mode, int) else mode
        url = self._file_path("/content", path, root, mode=mode_text)
        with self._client._open("PUT", url, _upload(data), "application/octet-stream") as res:
            return _file_info(json.loads(self._client._read(res).decode()))

    def delete_file(self, path: str, recursive: bool = False, root: bool = False) -> FileInfo:
        """Deletes a file, or with recursive, a directory and everything in it. Returns what was deleted."""
        return _file_info(self._client._call("DELETE", self._file_path("", path, root, recursive=recursive)))

    def download_archive(self, path: str, root: bool = False) -> bytes:
        """A directory's contents, or one file, as a tar.gz. Files the user can't read are left out."""
        with self._client._open("GET", self._file_path("/archive", path, root)) as res:
            return self._client._read(res)

    def upload_archive(self, path: str, data: Union[bytes, IO[bytes]], root: bool = False) -> ArchiveResult:
        """Unpacks a tar.gz (bytes or a binary file) into a directory, making it if need be."""
        with self._client._open("PUT", self._file_path("/archive", path, root), _upload(data), "application/gzip") as res:
            r = json.loads(self._client._read(res).decode())
        return ArchiveResult(path=r["path"], entries=r["entries"])

    def fork(
        self,
        count: int = 1,
        name: Optional[str] = None,
        size: Optional[str] = None,
        idle_minutes: Optional[int] = None,
    ) -> List["Sandbox"]:
        """Makes sandboxes (1 to 10) that start from a copy of this one's whole disk.

        A running sandbox is saved first, in seconds, and keeps running. With
        count above 1, name is the start of <name>-1, <name>-2 and so on.
        Size and idle minutes default to this sandbox's. Forks that failed to
        start come back in state "error"; resume() tries again.
        """
        body = _drop_none(name=name, count=count, size=size, idleMinutes=idle_minutes)
        return [Sandbox(self._client, info) for info in self._client._call("POST", self._path("/fork"), body)]

    def save(self, name: str) -> Save:
        """Saves this running sandbox's disk under a name, to start new sandboxes from.

        Unlike its own save, a named save stays until it's deleted. The sandbox
        keeps running; saving takes about 15 seconds per GB of changes. A
        taken name's save is replaced. See Cloudex.start_from_save.
        """
        return _save(self._client._call("POST", self._path("/saves"), {"name": name}))

    def host(self, port: int) -> str:
        """An HTTPS link to a port, on the deployment's preview domain. Anyone with it can use it.

        The same port gives the same link until it's revoked. Servers that listen
        only on localhost work too.
        """
        return self._client._call("POST", self._path("/hosts"), {"port": port})["url"]

    def links(self) -> List[HostLink]:
        """The preview links handed out since v0.1.14 that still work; older ones
        aren't listed, and revoke_links() ends them too."""
        return [HostLink(l["port"], l["url"], l["createdAt"]) for l in self._client._call("GET", self._path("/hosts"))]

    def revoke_links(self, port: Optional[int] = None) -> Dict[str, Any]:
        """Makes a port's link stop working at once, or without a port, every link to
        the sandbox, the desktop's too. host() then gives a new link. Returns
        {"revoked": [ports], "all": bool}."""
        return self._client._call("DELETE", self._path("/hosts" if port is None else f"/hosts/{port}"))

    def desktop(self, size: Optional[str] = None) -> Desktop:
        """Starts the desktop (Chrome and a terminal), or resizes a running one, and links to its viewer.

        The link is on the preview domain, so the deployment needs one. The
        first start installs it, which takes a minute or two.
        """
        # Checked first: starting the desktop without a way to link to it would only fail afterwards.
        if not self._client.health().preview_zone:
            raise CloudexError(400, "preview links need a domain: run cloudex deploy --preview-domain <your domain>")
        d = self._client._call("POST", self._path("/desktop"), _drop_none(size=size))
        return Desktop(
            url=f"{self.host(d['port'])}/{d['path']}",
            port=d["port"],
            path=d["path"],
            size=d["size"],
            installed=d["installed"],
            started=d["started"],
        )

    def stop_desktop(self) -> None:
        self._client._call("DELETE", self._path("/desktop"))

    def settings(self) -> Settings:
        """The sandbox's environment variable names, files and repo."""
        return _settings(self._client._call("GET", self._path("/settings")))

    def update_settings(
        self, env: Optional[Dict[str, Optional[str]]] = None, files: Optional[Dict[str, Optional[str]]] = None
    ) -> Settings:
        """Sets, or with None removes, environment variables and files.

        A running sandbox gets the change at once, in new shells; a stopped one
        when it next starts.
        """
        return _settings(self._client._call("PATCH", self._path("/settings"), _drop_none(env=env, files=files)))

    def prompt(
        self,
        prompt: str,
        agent: Optional[str] = None,
        cwd: Optional[str] = None,
        model: Optional[str] = None,
    ) -> "Agent":
        """Starts Claude Code (or agent="codex") on a task, with no terminal, and returns at once.

        Follow it with events() or wait(), and continue the conversation with
        send(). cwd defaults to the home directory, model to the agent's own.
        With provider keys set, it needs no sign-in.
        """
        info = self._client._call("POST", self._path("/agents"), _drop_none(prompt=prompt, agent=agent, cwd=cwd, model=model))
        return Agent(self._client, self._path("/agents/" + info["id"]), info)

    def agents(self) -> List["Agent"]:
        """The agents started in this sandbox, newest first."""
        return [Agent(self._client, self._path("/agents/" + i["id"]), i) for i in self._client._call("GET", self._path("/agents"))]

    def agent(self, id: str) -> "Agent":
        """An agent by id (ag-xxxxxxxx)."""
        path = self._path("/agents/" + urllib.parse.quote(id, safe=""))
        return Agent(self._client, path, self._client._call("GET", path))


class Agent:
    """Claude Code or Codex working on a task in a sandbox.

    Its fields are as of the last call that returned it; refresh(), events()
    and wait() update them. info holds everything the API reports.
    """

    def __init__(self, client: Cloudex, path: str, info: Dict[str, Any]):
        self._client = client
        self._path = path
        self.info = info

    @property
    def id(self) -> str:
        return self.info["id"]

    @property
    def state(self) -> str:
        """running while a turn is in progress or messages are queued, idle otherwise."""
        return self.info["state"]

    @property
    def last_message(self) -> Optional[str]:
        """The agent's final message in the last finished turn."""
        return self.info.get("lastMessage")

    @property
    def error(self) -> Optional[str]:
        """Why the last turn failed."""
        return self.info.get("error")

    def __repr__(self) -> str:
        return f"<Agent {self.id} {self.info.get('agent')} {self.state}>"

    def refresh(self) -> "Agent":
        self.info = self._client._call("GET", self._path)
        return self

    def send(self, prompt: str, interrupt: bool = False) -> "Agent":
        """Continues the conversation.

        While a turn runs the message waits for it; with interrupt, the running
        turn stops and queued messages are dropped first.
        """
        body = {"prompt": prompt, "interrupt": True} if interrupt else {"prompt": prompt}
        self.info = self._client._call("POST", self._path + "/messages", body)
        return self

    def interrupt(self) -> "Agent":
        """Stops the running turn and drops queued messages."""
        self.info = self._client._call("POST", self._path + "/interrupt")
        return self

    def events(self, after: int = 0, follow: bool = True) -> Iterator[Dict[str, Any]]:
        """The agent's events (dicts) from after (default 0, the start).

        With follow (the default), waits for new ones until the agent is idle;
        without, stops at the newest. Each request waits up to 25 seconds, so a
        client timeout must be longer than that.
        """
        wait = 25 if follow else 0
        while True:
            page = self._client._call("GET", f"{self._path}/events?after={after}&wait={wait}")
            self.info = page["agent"]
            yield from page["events"]
            after = page["next"]
            if after >= self.info["events"] and (not follow or self.info["state"] == "idle"):
                return

    def wait(self) -> "Agent":
        """Waits until the agent is idle; then last_message and error tell how its last turn went."""
        for _ in self.events(after=self.info["events"]):
            pass
        return self
