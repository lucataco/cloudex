import io
import json
import os
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

from cloudex import (
    ArchiveResult, Cloudex, CloudexError, ExecEvent, ExecResult, FileEntry, FileInfo, HostLink, ProviderKey, Settings,
)


def info(id, **extra):
    base = {
        "id": id, "name": None, "size": "standard-2", "state": "running", "idleMinutes": 30,
        "createdAt": 1, "updatedAt": 1, "startedAt": 1, "stoppedAt": None,
        "savedAt": None, "savedBytes": None, "error": None,
    }
    base.update(extra)
    return base


class FakeAPI(BaseHTTPRequestHandler):
    """Answers by "METHOD /path" from routes and records what it was sent.

    A body that isn't JSON is kept as bytes, with its type. An answer in bytes
    is sent as is; a route can also be a function that answers by itself.
    """

    routes = {}
    seen = []

    def _handle(self):
        length = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(length) if length else b""
        kind = self.headers.get("Content-Type")
        route = f"{self.command} {self.path}"
        seen = {"route": route, "auth": self.headers.get("Authorization"), "body": None}
        if raw and kind == "application/json":
            seen["body"] = json.loads(raw)
        elif raw:
            seen.update(body=raw, type=kind)
        FakeAPI.seen.append(seen)
        answer = FakeAPI.routes.get(route, (404, {"error": f"no route {route}"}))
        if callable(answer):
            return answer(self)
        status, body = answer
        data = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/octet-stream" if isinstance(body, bytes) else "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = _handle

    def log_message(self, *args):
        pass


class CloudexTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = ThreadingHTTPServer(("127.0.0.1", 0), FakeAPI)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.url = f"http://127.0.0.1:{cls.server.server_address[1]}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def setUp(self):
        FakeAPI.routes = {}
        FakeAPI.seen = []
        env = {k: v for k, v in os.environ.items() if not k.startswith("CLOUDEX_")}
        patcher = mock.patch.dict(os.environ, env, clear=True)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.cx = Cloudex(self.url + "/", "k1")

    def test_create_sends_options_with_the_key(self):
        FakeAPI.routes["POST /v1/sandboxes"] = (201, info("cx-aaa111", name="web"))
        box = self.cx.create(name="web", idle_minutes=5)
        self.assertEqual((box.id, box.name, box.state), ("cx-aaa111", "web", "running"))
        self.assertEqual(
            FakeAPI.seen, [{"route": "POST /v1/sandboxes", "auth": "Bearer k1", "body": {"name": "web", "idleMinutes": 5}}]
        )

    def test_api_errors_raise_with_status_and_message(self):
        FakeAPI.routes["GET /v1/sandboxes/nope"] = (404, {"error": "no sandbox named nope"})
        with self.assertRaises(CloudexError) as caught:
            self.cx.get("nope")
        self.assertEqual((caught.exception.status, str(caught.exception)), (404, "no sandbox named nope"))

    def test_lifecycle_updates_the_fields(self):
        FakeAPI.routes["GET /v1/sandboxes/my%20box"] = (200, info("cx-aaa111", name="my box"))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/stop"] = (200, info("cx-aaa111", state="stopped", savedAt=9))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/resume"] = (200, info("cx-aaa111", notice="restored"))
        FakeAPI.routes["DELETE /v1/sandboxes/cx-aaa111"] = (200, {"deleted": "cx-aaa111"})
        box = self.cx.get("my box")
        self.assertEqual(box.stop().state, "stopped")
        self.assertEqual(box.info["savedAt"], 9)
        box.resume(from_backup=True)
        self.assertEqual(box.info["notice"], "restored")
        box.delete()
        self.assertEqual(FakeAPI.seen[-2]["body"], {"fromBackup": True})
        self.assertEqual(FakeAPI.seen[-1]["route"], "DELETE /v1/sandboxes/cx-aaa111")

    def test_update_resume_size_and_links(self):
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111", name="web"))
        FakeAPI.routes["PATCH /v1/sandboxes/cx-aaa111"] = (200, info("cx-aaa111", name="site", idleMinutes=120))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/resume"] = (200, info("cx-aaa111", size="standard-4"))
        FakeAPI.routes["GET /v1/sandboxes/cx-aaa111/hosts"] = (200, [{"port": 3000, "url": "https://x", "createdAt": 1}])
        FakeAPI.routes["DELETE /v1/sandboxes/cx-aaa111/hosts/3000"] = (200, {"revoked": [3000], "all": False})
        FakeAPI.routes["DELETE /v1/sandboxes/cx-aaa111/hosts"] = (200, {"revoked": [3000], "all": True})
        box = self.cx.get("web")
        self.assertEqual(box.update(name="site", idle_minutes=120).name, "site")
        self.assertEqual(box.resume(size="standard-4").info["size"], "standard-4")
        self.assertEqual(box.links(), [HostLink(3000, "https://x", 1)])
        self.assertEqual(box.revoke_links(3000), {"revoked": [3000], "all": False})
        self.assertTrue(box.revoke_links()["all"])
        self.assertEqual(
            [(s["route"], s["body"]) for s in FakeAPI.seen[1:3]],
            [("PATCH /v1/sandboxes/cx-aaa111", {"name": "site", "idleMinutes": 120}),
             ("POST /v1/sandboxes/cx-aaa111/resume", {"size": "standard-4"})],
        )
        self.assertEqual(FakeAPI.seen[-1]["route"], "DELETE /v1/sandboxes/cx-aaa111/hosts")

    def test_exec_quotes_a_list(self):
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/exec"] = (
            200, {"exitCode": 1, "stdout": "", "stderr": "no", "timedOut": False, "ms": 3},
        )
        box = self.cx.get("web")
        result = box.exec(["echo", "it's", "a b"], root=True, timeout_sec=5)
        self.assertEqual(result, ExecResult(exit_code=1, stdout="", stderr="no", timed_out=False, ms=3))
        self.assertEqual(FakeAPI.seen[-1]["body"], {"cmd": "echo 'it'\"'\"'s' 'a b'", "root": True, "timeoutSec": 5})
        box.exec("ls | wc -l")
        self.assertEqual(FakeAPI.seen[-1]["body"], {"cmd": "ls | wc -l"})

    def test_fork_returns_working_sandboxes(self):
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/fork"] = (201, [
            info("cx-fff111", name="a-1", forkedFrom="cx-aaa111"),
            info("cx-fff222", name="a-2", state="error", error="no room"),
        ])
        FakeAPI.routes["POST /v1/sandboxes/cx-fff111/stop"] = (200, info("cx-fff111", state="stopped"))
        forks = self.cx.get("web").fork(count=2, name="a")
        self.assertEqual(FakeAPI.seen[-1]["body"], {"name": "a", "count": 2})
        self.assertEqual([(f.name, f.state, f.forked_from) for f in forks], [("a-1", "running", "cx-aaa111"), ("a-2", "error", None)])
        self.assertEqual(forks[0].stop().state, "stopped")

    def test_named_saves(self):
        save = {
            "name": "base", "id": "sv-aaaaaaaaaa", "sandbox": "cx-aaa111", "kind": "disk", "size": "standard-2",
            "idleMinutes": 30, "files": 12, "rawBytes": 2048, "bytes": 1024, "createdAt": 1, "ms": 900,
        }
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/saves"] = (201, dict(save, replaced=True))
        FakeAPI.routes["GET /v1/saves"] = (200, [save])
        FakeAPI.routes["POST /v1/saves/base/sandboxes"] = (201, [info("cx-bbb111", name="b-1", fromSave="base")])
        FakeAPI.routes["DELETE /v1/saves/base"] = (200, {"deleted": "base"})
        made = self.cx.get("web").save("base")
        self.assertEqual(FakeAPI.seen[-1]["body"], {"name": "base"})
        self.assertEqual((made.kind, made.raw_bytes, made.replaced), ("disk", 2048, True))
        self.assertEqual([s.name for s in self.cx.list_saves()], ["base"])
        self.assertIsNone(self.cx.list_saves()[0].replaced)
        boxes = self.cx.start_from_save("base", count=2, name="b")
        self.assertEqual(FakeAPI.seen[-1]["body"], {"name": "b", "count": 2})
        self.assertEqual([(b.name, b.from_save) for b in boxes], [("b-1", "base")])
        self.cx.delete_save("base")
        self.assertEqual(FakeAPI.seen[-1]["route"], "DELETE /v1/saves/base")
        with self.assertRaises(CloudexError) as e:
            self.cx.get_save("gone")
        self.assertEqual(e.exception.status, 404)

    def test_desktop_links_to_the_viewer(self):
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes["GET /v1/health"] = (200, {"ok": True, "version": "v0", "previewZone": "example.com"})
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/desktop"] = (
            200, {"port": 6080, "path": "vnc.html?autoconnect=1", "size": "1280x720", "installed": False, "started": True},
        )
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/hosts"] = (200, {"url": "https://6080-cx-aaa111-tok.example.com"})
        d = self.cx.get("web").desktop()
        self.assertEqual(d.url, "https://6080-cx-aaa111-tok.example.com/vnc.html?autoconnect=1")
        self.assertEqual(FakeAPI.seen[-2]["body"], {})

    def test_environment_then_saved_deployment(self):
        FakeAPI.routes["GET /v1/health"] = (200, {"ok": True, "version": "v0", "previewZone": None})
        os.environ["CLOUDEX_API_URL"] = self.url
        os.environ["CLOUDEX_API_KEY"] = "from-env"
        Cloudex().health()
        self.assertEqual(FakeAPI.seen[-1]["auth"], "Bearer from-env")
        del os.environ["CLOUDEX_API_URL"], os.environ["CLOUDEX_API_KEY"]
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "config.json"), "w") as f:
                json.dump({"apiUrl": self.url, "apiKey": "from-config"}, f)
            os.environ["CLOUDEX_CONFIG_DIR"] = d
            self.assertEqual(Cloudex().health().version, "v0")
        self.assertEqual(FakeAPI.seen[-1]["auth"], "Bearer from-config")

    def test_no_deployment_is_a_clear_error(self):
        with tempfile.TemporaryDirectory() as d:
            os.environ["CLOUDEX_CONFIG_DIR"] = d
            with self.assertRaises(CloudexError) as caught:
                Cloudex()
        self.assertEqual(caught.exception.status, 0)
        self.assertIn("cloudex login", str(caught.exception))

    def test_provider_keys(self):
        FakeAPI.routes["PUT /v1/keys/anthropic"] = (200, {"provider": "anthropic", "hint": "…wxyz", "updatedAt": 5})
        FakeAPI.routes["GET /v1/keys"] = (200, [{"provider": "anthropic", "hint": "…wxyz", "updatedAt": 5}])
        FakeAPI.routes["DELETE /v1/keys/anthropic"] = (200, {"deleted": "anthropic"})
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111", providerKeys=["anthropic"]))
        self.assertEqual(self.cx.set_key("anthropic", "sk-ant-x-wxyz"), ProviderKey("anthropic", "…wxyz", 5))
        self.assertEqual(FakeAPI.seen[-1]["body"], {"key": "sk-ant-x-wxyz"})
        self.assertEqual([k.provider for k in self.cx.list_keys()], ["anthropic"])
        self.assertEqual(self.cx.get("web").provider_keys, ["anthropic"])
        self.cx.delete_key("anthropic")
        self.assertEqual(FakeAPI.seen[-1]["route"], "DELETE /v1/keys/anthropic")

    def test_tokens(self):
        t = {"id": "tk-aaaa2222", "name": "ci", "kind": "token", "role": "user", "ownOnly": True, "createdAt": 1,
             "createdBy": "admin", "expiresAt": None, "lastUsedAt": None}
        FakeAPI.routes["POST /v1/tokens"] = (201, dict(t, secret="cx_aaaa2222_s"))
        FakeAPI.routes["GET /v1/tokens"] = (200, [t])
        FakeAPI.routes["DELETE /v1/tokens/tk-aaaa2222"] = (200, {"revoked": "tk-aaaa2222"})
        FakeAPI.routes["GET /v1/me"] = (200, {"role": "user", "ownOnly": True, "token": t})
        made = self.cx.create_token("ci", own_only=True, expires_in_sec=3600)
        self.assertEqual((made.id, made.own_only, made.secret), ("tk-aaaa2222", True, "cx_aaaa2222_s"))
        self.assertEqual(FakeAPI.seen[-1]["body"], {"name": "ci", "role": "user", "ownOnly": True, "expiresInSec": 3600})
        self.assertEqual([x.secret for x in self.cx.list_tokens()], [None])
        me = self.cx.me()
        self.assertEqual((me.role, me.own_only, me.token.id), ("user", True, "tk-aaaa2222"))
        self.cx.revoke_token("tk-aaaa2222")
        self.assertEqual(FakeAPI.seen[-1]["route"], "DELETE /v1/tokens/tk-aaaa2222")

    def test_agents(self):
        def agent(**extra):
            base = {"id": "ag-aaaa1111", "agent": "claude", "state": "running", "cwd": "/home/cloudex", "model": None,
                    "turns": 1, "queued": 0, "events": 1, "sessionId": None, "lastMessage": None,
                    "lastExitCode": None, "error": None, "createdAt": 1, "updatedAt": 1}
            base.update(extra)
            return base

        path = "/v1/sandboxes/cx-aaa111/agents/ag-aaaa1111"
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/agents"] = (201, agent(turns=0, events=0))
        FakeAPI.routes[f"GET {path}/events?after=0&wait=25"] = (200, {
            "events": [{"seq": 1, "turn": 1, "at": 1, "type": "prompt", "text": "fix it"}],
            "next": 1, "agent": agent(events=2)})
        FakeAPI.routes[f"GET {path}/events?after=1&wait=25"] = (200, {
            "events": [{"seq": 2, "turn": 1, "at": 2, "type": "end", "exitCode": 0}],
            "next": 2, "agent": agent(state="idle", events=2, lastMessage="fixed")})
        FakeAPI.routes[f"GET {path}/events?after=2&wait=25"] = (200, {"events": [], "next": 2, "agent": agent(state="idle", events=2, lastMessage="fixed")})
        FakeAPI.routes[f"POST {path}/messages"] = (200, agent(queued=1))
        FakeAPI.routes[f"POST {path}/interrupt"] = (200, agent(state="idle", error="interrupted"))

        a = self.cx.get("web").prompt("fix it", agent="codex")
        self.assertEqual(FakeAPI.seen[-1]["body"], {"prompt": "fix it", "agent": "codex"})
        self.assertEqual([e["type"] for e in a.events()], ["prompt", "end"])
        self.assertEqual((a.state, a.last_message), ("idle", "fixed"))
        self.assertIs(a.wait(), a)
        self.assertEqual(FakeAPI.seen[-1]["route"], f"GET {path}/events?after=2&wait=25")
        a.send("and the docs", interrupt=True)
        self.assertEqual(FakeAPI.seen[-1]["body"], {"prompt": "and the docs", "interrupt": True})
        self.assertEqual(a.interrupt().error, "interrupted")

    def test_settings(self):
        view = {"env": ["A"], "files": ["~/.npmrc"], "repo": None}
        FakeAPI.routes["POST /v1/sandboxes"] = (201, info("cx-aaa111"))
        FakeAPI.routes["GET /v1/sandboxes/cx-aaa111/settings"] = (200, view)
        FakeAPI.routes["PATCH /v1/sandboxes/cx-aaa111/settings"] = (200, dict(view, env=[]))
        box = self.cx.create(env={"A": "1"}, files={"~/.npmrc": "x"}, repo="https://github.com/you/app#dev")
        self.assertEqual(FakeAPI.seen[-1]["body"], {"env": {"A": "1"}, "files": {"~/.npmrc": "x"}, "repo": "https://github.com/you/app#dev"})
        self.assertEqual(box.settings(), Settings(env=["A"], files=["~/.npmrc"], repo=None))
        self.assertEqual(box.update_settings(env={"A": None}).env, [])
        self.assertEqual(FakeAPI.seen[-1]["body"], {"env": {"A": None}})

    def test_exec_stream_yields_output_as_it_comes_skipping_keepalives(self):
        def stream(h):
            h.send_response(200)
            h.send_header("Content-Type", "application/x-ndjson")
            h.end_headers()
            text = ('{"type":"stdout","data":"hi\\n"}\n{"type":"keepalive"}\n'
                    '{"type":"stderr","data":"\u00e9"}\n{"type":"exit","exitCode":3,"timedOut":false,"ms":9}\n')
            data = text.encode()
            for part in (data[:10], data[10:40], data[40:]):
                h.wfile.write(part)
                h.wfile.flush()
                time.sleep(0.01)

        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/exec"] = stream
        events = list(self.cx.get("web").exec_stream(["npm", "test"], cwd="~/app"))
        self.assertEqual(events, [
            ExecEvent("stdout", "hi\n"),
            ExecEvent("stderr", "\u00e9"),
            ExecEvent("exit", exit_code=3, timed_out=False, ms=9),
        ])
        self.assertEqual(FakeAPI.seen[-1]["body"], {"cmd": "npm test", "cwd": "~/app", "stream": True})

    def test_exec_stream_on_an_api_without_streaming(self):
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/exec"] = (
            200, {"exitCode": 0, "stdout": "out", "stderr": "", "timedOut": False, "ms": 4},
        )
        events = list(self.cx.get("web").exec_stream("echo out"))
        self.assertEqual(events, [ExecEvent("stdout", "out"), ExecEvent("exit", exit_code=0, timed_out=False, ms=4)])

    def test_exec_stream_raises_when_it_breaks_off_and_hangs_up_when_left(self):
        def broken(h):
            h.send_response(200)
            h.send_header("Content-Type", "application/x-ndjson")
            h.end_headers()
            h.wfile.write(b'{"type":"stdout","data":"a"}\n{"type":"error","error":"the sandbox stopped"}\n')

        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/exec"] = broken
        box = self.cx.get("web")
        got = []
        with self.assertRaises(CloudexError) as caught:
            for e in box.exec_stream("sleep 1"):
                got.append(e.data)
        self.assertEqual((caught.exception.status, str(caught.exception)), (0, "the sandbox stopped"))
        self.assertEqual(got, ["a"])

        hung_up = threading.Event()

        def endless(h):
            h.send_response(200)
            h.send_header("Content-Type", "application/x-ndjson")
            h.end_headers()
            try:
                for _ in range(500):
                    h.wfile.write(b'{"type":"stdout","data":"tick"}\n')
                    h.wfile.flush()
                    time.sleep(0.01)
            except OSError:
                hung_up.set()

        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/exec"] = endless
        for e in box.exec_stream("yes tick"):
            self.assertEqual(e.type, "stdout")
            break
        self.assertTrue(hung_up.wait(5))

    def test_commands(self):
        def command(**extra):
            base = {"id": "cm-aaaa1111", "cmd": "npm run build", "root": False, "cwd": "/home/cloudex/app",
                    "state": "running", "exitCode": None, "signal": None, "timedOut": False, "timeoutSec": 0,
                    "startedAt": 1, "endedAt": None, "outputBytes": 0}
            base.update(extra)
            return base

        def page(data, offset, nxt, **extra):
            return {"data": data, "offset": offset, "next": nxt, "command": command(**extra)}

        path = "/v1/sandboxes/cx-aaa111/commands/cm-aaaa1111"
        later = [page("", 4, 4, outputBytes=4), page("two\n", 4, 8, state="exited", exitCode=0, endedAt=2, outputBytes=8)]

        def answer_later(h):
            data = json.dumps(later.pop(0)).encode()
            h.send_response(200)
            h.send_header("Content-Type", "application/json")
            h.send_header("Content-Length", str(len(data)))
            h.end_headers()
            h.wfile.write(data)

        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes["POST /v1/sandboxes/cx-aaa111/commands"] = (201, command())
        FakeAPI.routes["GET /v1/sandboxes/cx-aaa111/commands"] = (200, [command()])
        FakeAPI.routes[f"GET {path}"] = (200, command(outputBytes=6))
        FakeAPI.routes[f"GET {path}/output?after=2&wait=0"] = (200, page("e\n", 2, 4, outputBytes=4))
        FakeAPI.routes[f"GET {path}/output?after=0&wait=25"] = (200, page("one\n", 0, 4, outputBytes=4))
        FakeAPI.routes[f"GET {path}/output?after=4&wait=25"] = answer_later
        FakeAPI.routes[f"POST {path}/kill"] = (200, command(state="killed", signal="INT", endedAt=5))
        FakeAPI.routes[f"DELETE {path}"] = (200, command(state="killed", signal="INT", endedAt=5))

        box = self.cx.get("web")
        cmd = box.start_command(["npm", "run", "build"], cwd="app", timeout_sec=60)
        self.assertEqual((cmd.id, cmd.state, cmd.exit_code), ("cm-aaaa1111", "running", None))
        self.assertEqual(FakeAPI.seen[-1]["body"], {"cmd": "npm run build", "cwd": "app", "timeoutSec": 60})
        self.assertEqual([c.id for c in box.list_commands()], ["cm-aaaa1111"])
        self.assertEqual(box.get_command(cmd.id).output_bytes, 6)
        self.assertEqual(box.command_output(cmd.id, after=2).data, "e\n")

        pages = list(box.follow_command(cmd.id))
        self.assertEqual([p.data for p in pages], ["one\n", "two\n"])
        self.assertEqual((pages[-1].command.state, pages[-1].command.exit_code), ("exited", 0))

        self.assertEqual(box.kill_command(cmd.id, "INT").signal, "INT")
        self.assertEqual(FakeAPI.seen[-1]["body"], {"signal": "INT"})
        box.kill_command(cmd.id)
        self.assertEqual(FakeAPI.seen[-1]["body"], {})
        self.assertEqual(box.forget_command(cmd.id).state, "killed")
        self.assertEqual(FakeAPI.seen[-1]["route"], f"DELETE {path}")

    def test_files(self):
        data = bytes(range(256))
        files = "/v1/sandboxes/cx-aaa111/files"

        def file_info(path, **extra):
            base = {"path": path, "type": "file", "size": 256, "mode": "644", "mtime": 1}
            base.update(extra)
            return base

        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes[f"PUT {files}/content?path=bin/my%20tool&mode=755"] = (200, file_info("/home/cloudex/bin/my tool", mode="755"))
        FakeAPI.routes[f"PUT {files}/content?path=~/.npmrc"] = (200, file_info("/home/cloudex/.npmrc", size=3))
        FakeAPI.routes[f"GET {files}/content?path=bin/my%20tool"] = (200, data)
        FakeAPI.routes[f"GET {files}?path=/etc&root=true"] = (200, file_info(
            "/etc", type="dir", size=4096, mode="755", entries=[{"name": "hosts", "type": "file", "size": 9, "mode": "644", "mtime": 1}],
        ))
        FakeAPI.routes[f"DELETE {files}?path=build&recursive=true"] = (200, file_info("/home/cloudex/build", type="dir"))
        FakeAPI.routes[f"GET {files}?path=gone"] = (404, {"error": "no such file: /home/cloudex/gone"})
        box = self.cx.get("web")

        self.assertEqual(box.write_file("bin/my tool", data, mode=0o755).mode, "755")
        self.assertEqual((FakeAPI.seen[-1]["body"], FakeAPI.seen[-1]["type"]), (data, "application/octet-stream"))
        box.write_file("bin/my tool", data, mode="755")
        self.assertEqual(FakeAPI.seen[-1]["route"], f"PUT {files}/content?path=bin/my%20tool&mode=755")
        box.write_file("~/.npmrc", "h\u00e9\n")
        self.assertEqual(FakeAPI.seen[-1]["body"], "h\u00e9\n".encode())
        f = io.BytesIO(b"skip" + data)
        f.seek(4)
        box.write_file("~/.npmrc", f)
        self.assertEqual(FakeAPI.seen[-1]["body"], data)
        self.assertEqual(box.read_file("bin/my tool"), data)

        etc = box.stat("/etc", root=True)
        self.assertEqual((etc.type, etc.truncated), ("dir", False))
        self.assertEqual(etc.entries, [FileEntry(name="hosts", type="file", size=9, mode="644", mtime=1)])
        self.assertEqual(box.delete_file("build", recursive=True), FileInfo(path="/home/cloudex/build", type="dir", size=256, mode="644", mtime=1))
        with self.assertRaises(CloudexError) as caught:
            box.stat("gone")
        self.assertEqual(caught.exception.status, 404)

    def test_archives(self):
        tgz = bytes([0x1F, 0x8B, 8, 0, 0, 0, 0, 0, 0, 3, 0xFF])
        files = "/v1/sandboxes/cx-aaa111/files"
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes[f"PUT {files}/archive?path=app"] = (200, {"path": "/home/cloudex/app", "entries": 12})
        FakeAPI.routes[f"GET {files}/archive?path=app"] = (200, tgz)
        box = self.cx.get("web")
        self.assertEqual(box.upload_archive("app", tgz), ArchiveResult(path="/home/cloudex/app", entries=12))
        self.assertEqual((FakeAPI.seen[-1]["body"], FakeAPI.seen[-1]["type"]), (tgz, "application/gzip"))
        self.assertEqual(box.download_archive("app"), tgz)

    def test_unreachable_api(self):
        for url, tried in (("http://127.0.0.1:9", "http://127.0.0.1:9"), ("127.0.0.1:9", "https://127.0.0.1:9")):
            with self.assertRaises(CloudexError) as caught:
                Cloudex(url, "k").list()
            self.assertEqual(caught.exception.status, 0)
            self.assertTrue(str(caught.exception).startswith(f"can't reach {tried}: "), str(caught.exception))

    def test_a_timeout_is_an_error_with_the_url(self):
        FakeAPI.routes["GET /v1/sandboxes"] = lambda handler: time.sleep(0.5)
        with self.assertRaises(CloudexError) as caught:
            Cloudex(self.url, "k", timeout=0.1).list()
        self.assertEqual(caught.exception.status, 0)
        self.assertTrue(str(caught.exception).startswith(f"can't reach {self.url}: "), str(caught.exception))

    def test_a_body_that_breaks_off_is_an_error_with_the_url(self):
        def cut_off(handler):
            handler.send_response(200)
            handler.send_header("Content-Type", "application/json")
            handler.send_header("Content-Length", "100")
            handler.end_headers()
            handler.wfile.write(b"[{")

        FakeAPI.routes["GET /v1/sandboxes"] = cut_off
        with self.assertRaises(CloudexError) as caught:
            self.cx.list()
        self.assertEqual(caught.exception.status, 0)
        self.assertTrue(str(caught.exception).startswith(f"can't reach {self.url}: "), str(caught.exception))

    def test_an_html_error_page_becomes_its_status_and_title(self):
        def page(status, body, kind="text/html"):
            def answer(handler):
                data = body.encode()
                handler.send_response(status)
                handler.send_header("Content-Type", kind)
                handler.send_header("Content-Length", str(len(data)))
                handler.end_headers()
                handler.wfile.write(data)

            return answer

        FakeAPI.routes["GET /v1/sandboxes"] = page(502, "<html>\n<head><title>502 Bad Gateway</title></head>\n<body>nginx</body></html>")
        FakeAPI.routes["GET /v1/saves"] = page(503, "<html><title>\n Just a  moment </title></html>")
        FakeAPI.routes["GET /v1/keys"] = page(500, "x" * 600, "text/plain")
        with self.assertRaises(CloudexError) as caught:
            self.cx.list()
        self.assertEqual((caught.exception.status, str(caught.exception)), (502, "502 Bad Gateway"))
        with self.assertRaises(CloudexError) as caught:
            self.cx.list_saves()
        self.assertEqual(str(caught.exception), "503 Just a moment")
        with self.assertRaises(CloudexError) as caught:
            self.cx.list_keys()
        self.assertEqual(str(caught.exception), "x" * 500 + "\u2026")

    def test_an_unknown_route_says_the_deployment_is_older(self):
        FakeAPI.routes["GET /v1/saves"] = (404, {"error": "not found"})
        FakeAPI.routes["GET /v1/saves/gone"] = (404, {"error": "no save named gone"})
        with self.assertRaises(CloudexError) as caught:
            self.cx.list_saves()
        self.assertEqual(caught.exception.status, 404)
        self.assertRegex(str(caught.exception), r"older than the SDK.*cloudex deploy")
        with self.assertRaises(CloudexError) as caught:
            self.cx.get_save("gone")
        self.assertEqual(str(caught.exception), "no save named gone")

    def test_the_saved_key_goes_only_to_the_saved_deployment(self):
        FakeAPI.routes["GET /v1/health"] = (200, {"ok": True, "version": "v0", "previewZone": None})
        other = self.url.replace("127.0.0.1", "localhost")
        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "config.json"), "w") as f:
                json.dump({"apiUrl": self.url + "/", "apiKey": "saved"}, f)
            os.environ["CLOUDEX_CONFIG_DIR"] = d

            Cloudex(self.url).health()
            self.assertEqual(FakeAPI.seen[-1]["auth"], "Bearer saved")

            FakeAPI.seen.clear()
            with self.assertRaisesRegex(CloudexError, r"no API key for http://localhost:\d+: pass api_key or set CLOUDEX_API_KEY") as caught:
                Cloudex(other)
            self.assertEqual(caught.exception.status, 0)
            os.environ["CLOUDEX_API_URL"] = other
            with self.assertRaisesRegex(CloudexError, "no API key"):
                Cloudex()
            self.assertEqual(FakeAPI.seen, [])

            Cloudex(other, "mine").health()
            self.assertEqual(FakeAPI.seen[-1]["auth"], "Bearer mine")
            os.environ["CLOUDEX_API_KEY"] = "from-env"
            Cloudex().health()
            self.assertEqual(FakeAPI.seen[-1]["auth"], "Bearer from-env")

    def test_the_saved_key_goes_to_the_saved_url_however_either_is_written(self):
        sent = []

        def urlopen(req, timeout=None):
            sent.append((req.full_url, req.get_header("Authorization")))
            raise OSError("not sent")

        with tempfile.TemporaryDirectory() as d:
            with open(os.path.join(d, "config.json"), "w") as f:
                json.dump({"apiUrl": "https://Cloudex-API.x.workers.dev:443/", "apiKey": "saved"}, f)
            os.environ["CLOUDEX_CONFIG_DIR"] = d
            with mock.patch("urllib.request.urlopen", urlopen):
                for url in (" CLOUDEX-API.X.WORKERS.DEV ", "HTTPS://cloudex-api.x.workers.dev//", "https://cloudex-api.x.workers.dev:443"):
                    with self.assertRaises(CloudexError):
                        Cloudex(url).health()
                    self.assertEqual(sent.pop(), ("https://cloudex-api.x.workers.dev/v1/health", "Bearer saved"), url)
            with self.assertRaisesRegex(CloudexError, "no API key for http:"):
                Cloudex("http://cloudex-api.x.workers.dev")
        self.assertEqual(sent, [])

    def test_desktop_without_a_preview_domain_fails_before_starting_anything(self):
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        FakeAPI.routes["GET /v1/health"] = (200, {"ok": True, "version": "v0", "previewZone": None})
        box = self.cx.get("web")
        with self.assertRaisesRegex(CloudexError, "preview links need a domain") as caught:
            box.desktop()
        self.assertEqual(caught.exception.status, 400)
        self.assertFalse(any("/desktop" in seen["route"] for seen in FakeAPI.seen))

    def test_a_file_mode_below_0o100_is_sent_with_three_digits(self):
        files = "/v1/sandboxes/cx-aaa111/files"
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
        for mode in ("077", "007"):
            FakeAPI.routes[f"PUT {files}/content?path=f&mode={mode}"] = (
                200, {"path": "/home/cloudex/f", "type": "file", "size": 1, "mode": mode, "mtime": 1},
            )
        box = self.cx.get("web")
        self.assertEqual(box.write_file("f", "x", mode=0o77).mode, "077")
        self.assertEqual(box.write_file("f", "x", mode=7).mode, "007")


if __name__ == "__main__":
    unittest.main()
