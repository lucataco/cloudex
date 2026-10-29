"""The Cloudex SDK: create, fork and drive sandboxes from Python.

It talks to the /v1 API that `cloudex deploy` set up in your Cloudflare
account, using only the standard library.

    from cloudex import Cloudex

    cx = Cloudex()  # the deployment saved by `cloudex login`, or $CLOUDEX_API_URL and $CLOUDEX_API_KEY
    box = cx.create(name="web")
    print(box.exec("uname -a").stdout)
    forks = box.fork(count=3, name="try")
"""

from __future__ import annotations

import json
import os
import shlex
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Dict, List, Optional, Sequence, Union

__all__ = ["Cloudex", "CloudexError", "Desktop", "ExecResult", "Health", "Sandbox"]
__version__ = "0.1.6"


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
class Desktop:
    url: str  # the viewer, on the preview domain; anyone with it can use the desktop
    port: int
    path: str
    size: str
    installed: bool  # this call installed it (the first start takes a minute or two)
    started: bool  # this call started it, rather than finding it running


@dataclass(frozen=True)
class Health:
    ok: bool
    version: str
    preview_zone: Optional[str]
    backups: bool


def _drop_none(**fields: Any) -> Dict[str, Any]:
    return {k: v for k, v in fields.items() if v is not None}


def _saved_deployment(config_dir: Optional[str]) -> Dict[str, Any]:
    # The CLI keeps its deployment in ~/.config/cloudex/config.json.
    path = Path(config_dir) if config_dir else Path.home() / ".config" / "cloudex"
    try:
        return json.loads((path / "config.json").read_text())
    except (OSError, ValueError):
        return {}


class Cloudex:
    """A Cloudex deployment.

    The URL and key come from the arguments, then $CLOUDEX_API_URL and
    $CLOUDEX_API_KEY, then the deployment saved by `cloudex deploy` or
    `cloudex login`, the same order as the CLI. timeout is in seconds per
    request; by default there's none, since starting or forking can take
    minutes the first time on a host.
    """

    def __init__(self, api_url: Optional[str] = None, api_key: Optional[str] = None, timeout: Optional[float] = None):
        url = api_url or os.environ.get("CLOUDEX_API_URL")
        key = api_key or os.environ.get("CLOUDEX_API_KEY")
        if not url or not key:
            saved = _saved_deployment(os.environ.get("CLOUDEX_CONFIG_DIR"))
            url = url or saved.get("apiUrl")
            key = key or saved.get("apiKey")
        if not url:
            raise CloudexError(
                0,
                "no Cloudex deployment: pass api_url and api_key, set CLOUDEX_API_URL and CLOUDEX_API_KEY, or run cloudex login",
            )
        self.api_url = url.rstrip("/")
        self._key = key or ""
        self._timeout = timeout

    def __repr__(self) -> str:
        return f"Cloudex({self.api_url!r})"

    def health(self) -> Health:
        """The deployment's version and features."""
        h = self._call("GET", "/v1/health")
        return Health(ok=h["ok"], version=h["version"], preview_zone=h.get("previewZone"), backups=bool(h.get("backups")))

    def list(self) -> List["Sandbox"]:
        """All sandboxes, newest first."""
        return [Sandbox(self, info) for info in self._call("GET", "/v1/sandboxes")]

    def get(self, ref: str) -> "Sandbox":
        """A sandbox by id (cx-xxxxxx) or name."""
        return Sandbox(self, self._call("GET", _sandbox_path(ref)))

    def create(self, name: Optional[str] = None, size: Optional[str] = None, idle_minutes: Optional[int] = None) -> "Sandbox":
        """Creates a sandbox and returns once it's running, usually in about 10 seconds.

        size is standard-1 to standard-4 (default standard-2). idle_minutes
        (default 30) is how long without activity before it saves and stops;
        0 waits for the 6-hour platform limit.
        """
        body = _drop_none(name=name, size=size, idleMinutes=idle_minutes)
        return Sandbox(self, self._call("POST", "/v1/sandboxes", body))

    def _call(self, method: str, path: str, body: Optional[Dict[str, Any]] = None) -> Any:
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.api_url + path, data=data, method=method)
        req.add_header("Authorization", f"Bearer {self._key}")
        req.add_header("User-Agent", f"cloudex-python/{__version__}")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        try:
            with urllib.request.urlopen(req, timeout=self._timeout) as res:
                text = res.read().decode()
        except urllib.error.HTTPError as e:
            text = e.read().decode(errors="replace")
            try:
                message = json.loads(text)["error"]
            except (ValueError, KeyError, TypeError):
                message = text.strip() or e.reason
            raise CloudexError(e.code, str(message)) from None
        except urllib.error.URLError as e:
            raise CloudexError(0, f"can't reach {self.api_url}: {e.reason}") from None
        return json.loads(text) if text else None


def _sandbox_path(ref: str) -> str:
    return "/v1/sandboxes/" + urllib.parse.quote(ref, safe="")


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

    def __repr__(self) -> str:
        label = f" {self.name!r}" if self.name else ""
        return f"<Sandbox {self.id}{label} {self.state}>"

    def _path(self, suffix: str = "") -> str:
        return _sandbox_path(self.id) + suffix

    def _update(self, method: str, suffix: str, body: Optional[Dict[str, Any]] = None) -> "Sandbox":
        self.info = self._client._call(method, self._path(suffix), body)
        return self

    def refresh(self) -> "Sandbox":
        return self._update("GET", "")

    def stop(self) -> "Sandbox":
        """Saves the disk, backs up /home and stops."""
        return self._update("POST", "/stop")

    def resume(self, from_backup: bool = False) -> "Sandbox":
        """Starts from the save, or with from_backup, from a fresh image with /home restored."""
        return self._update("POST", "/resume", {"fromBackup": True} if from_backup else None)

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
        element. A failing command doesn't raise: check exit_code.
        """
        cmd = command if isinstance(command, str) else shlex.join(command)
        body = _drop_none(cmd=cmd, root=root or None, cwd=cwd, timeoutSec=timeout_sec)
        r = self._client._call("POST", self._path("/exec"), body)
        return ExecResult(exit_code=r["exitCode"], stdout=r["stdout"], stderr=r["stderr"], timed_out=r["timedOut"], ms=r["ms"])

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

    def host(self, port: int) -> str:
        """An HTTPS link to a port, on the deployment's preview domain. Anyone with it can use it."""
        return self._client._call("POST", self._path("/hosts"), {"port": port})["url"]

    def desktop(self, size: Optional[str] = None) -> Desktop:
        """Starts the desktop (Chrome and a terminal), or resizes a running one, and links to its viewer.

        The first start installs it, which takes a minute or two.
        """
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
