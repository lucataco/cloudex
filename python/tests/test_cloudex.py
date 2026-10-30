import json
import os
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from unittest import mock

from cloudex import Cloudex, CloudexError, ExecResult, ProviderKey, Settings


def info(id, **extra):
    base = {
        "id": id, "name": None, "size": "standard-2", "state": "running", "idleMinutes": 30,
        "createdAt": 1, "updatedAt": 1, "startedAt": 1, "stoppedAt": None,
        "savedAt": None, "savedBytes": None, "error": None,
    }
    base.update(extra)
    return base


class FakeAPI(BaseHTTPRequestHandler):
    """Answers by "METHOD /path" from routes and records what it was sent."""

    routes = {}
    seen = []

    def _handle(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = json.loads(self.rfile.read(length)) if length else None
        route = f"{self.command} {self.path}"
        FakeAPI.seen.append({"route": route, "auth": self.headers.get("Authorization"), "body": body})
        status, answer = FakeAPI.routes.get(route, (404, {"error": f"no route {route}"}))
        data = json.dumps(answer).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
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

    def test_desktop_links_to_the_viewer(self):
        FakeAPI.routes["GET /v1/sandboxes/web"] = (200, info("cx-aaa111"))
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

    def test_unreachable_api(self):
        with self.assertRaises(CloudexError) as caught:
            Cloudex("http://127.0.0.1:9", "k").list()
        self.assertEqual(caught.exception.status, 0)


if __name__ == "__main__":
    unittest.main()
