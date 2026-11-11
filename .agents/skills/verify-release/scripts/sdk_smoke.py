"""The Python SDK against a live deployment: python3 sdk_smoke.py <name prefix>.

Uses the deployment in $CLOUDEX_CONFIG_DIR. Deletes what it makes.
"""
import os
import sys
import time
import urllib.error
import urllib.request

from cloudex import Cloudex, CloudexError

prefix = sys.argv[1]
# With $VERIFY_NODE, its sandbox runs on that node, and what isn't on nodes in
# v0.2.2 (forks, named saves, agents) must be refused with a 501 instead.
node = os.environ.get("VERIFY_NODE") or None


def refused(what, call):
    try:
        call()
    except CloudexError as e:
        assert e.status == 501, f"{what} on a node: {e.status} {e}"
        print("skip", what, "(not on nodes in v0.2.2; refused with 501 as it should be)")
        return
    raise AssertionError(f"{what} on a node wasn't refused")
cx = Cloudex()
made, saves = [], []


def step(s):
    print("ok", s, flush=True)


def fetch(url):
    try:
        # Cloudflare's Browser Integrity Check, on by default in many zones, refuses urllib's user agent.
        req = urllib.request.Request(url, headers={"User-Agent": "cloudex-verify"})
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, ""


try:
    health = cx.health()
    assert health.version, "health has a version"
    step("health")

    box = cx.create(name=f"{prefix}-a", env={"SDK_VAR": "py"}, files={"sdk.txt": "from-py\n"}, node=node)
    made.append(box)
    assert box.state == "running"
    assert cx.get(f"{prefix}-a").id == box.id
    assert any(b.id == box.id for b in cx.list())
    step("create, get, list")

    r = box.exec("echo $SDK_VAR; cat ~/sdk.txt; echo err >&2; exit 4")
    assert r.exit_code == 4, r
    assert r.stdout == "py\nfrom-py\n", r
    assert r.stderr == "err\n", r
    assert box.exec(["python3", "-c", "print('a b')"]).stdout == "a b\n"
    step("exec, env and files")

    streamed, code = "", None
    for e in box.exec_stream("for i in 1 2 3; do echo $i; done; exit 2"):
        if e.type == "exit":
            code = e.exit_code
        else:
            streamed += e.data
    assert streamed == "1\n2\n3\n" and code == 2, (streamed, code)
    step("exec_stream")

    cmd = box.start_command("echo bg; exit 6")
    out, last = "", None
    for part in box.follow_command(cmd.id):
        out += part.data
        last = part.command
    assert out == "bg\n" and last.exit_code == 6, (out, last)
    sleeper = box.start_command("sleep 600")
    box.kill_command(sleeper.id)
    assert any(c.id == sleeper.id for c in box.list_commands())
    assert box.get_command(sleeper.id).state != "running"
    step("start_command, follow_command, kill_command")

    box.write_file("d/f.txt", "file-body")
    assert box.read_file("d/f.txt") == b"file-body"
    assert any(e.name == "f.txt" for e in box.stat("d").entries)
    box.upload_archive("d2", box.download_archive("d"))
    assert box.read_file("d2/f.txt") == b"file-body"
    box.delete_file("d", recursive=True)
    try:
        box.read_file("d/f.txt")
        raise AssertionError("read of a deleted file worked")
    except CloudexError:
        pass
    step("files")

    box.update_settings(env={"SDK_TWO": "2"})
    assert box.exec("echo $SDK_TWO").stdout == "2\n"
    assert "SDK_TWO" in box.settings().env
    box.update(idle_minutes=90)
    assert box.refresh().info["idleMinutes"] == 90
    step("settings and update")

    if health.preview_zone:
        box.start_command("mkdir -p /tmp/w && echo hosted > /tmp/w/index.html && cd /tmp/w && exec python3 -m http.server 8126 --bind 127.0.0.1")
        time.sleep(2)
        url = box.host(8126)
        assert fetch(url) == (200, "hosted\n"), fetch(url)
        assert any(link.url == url for link in box.links())
        box.revoke_links(8126)
        assert fetch(url)[0] == 404
        step("host, links, revoke_links")

    box.exec("echo forked > ~/marker")
    if node:
        assert box.placement["provider"] == "node" and box.placement["node"]["name"] == node
        assert any(n.name == node for n in cx.nodes.list(live=False))
        step("create on a node, placement, nodes.list")
        refused("fork", lambda: box.fork(count=2, name=f"{prefix}-f"))
        refused("save", lambda: box.save(f"{prefix}-save"))
    else:
        forks = box.fork(count=2, name=f"{prefix}-f")
        made += forks
        assert len(forks) == 2 and forks[1].exec("cat ~/marker").stdout == "forked\n"
        step("fork")

        save = box.save(f"{prefix}-save")
        saves.append(save.name)
        assert any(s.name == save.name for s in cx.list_saves())
        (from_save,) = cx.start_from_save(save.name, name=f"{prefix}-s")
        made.append(from_save)
        assert from_save.exec("cat ~/marker").stdout == "forked\n"
        step("save, list_saves, start_from_save")

    box.stop()
    assert box.state == "stopped"
    box.resume()
    assert box.state == "running"
    assert box.exec("cat ~/marker").stdout == "forked\n"
    step("stop and resume")

    try:
        cx.get(f"{prefix}-missing")
        raise AssertionError("get of a missing sandbox worked")
    except CloudexError as e:
        assert e.status == 404, e.status
    step("errors are CloudexError with status")

    if node:
        refused("prompt", lambda: box.prompt("Reply with exactly the word SDKOK and nothing else.", agent="codex"))
    elif any(k.provider == "openai" for k in cx.list_keys()):
        agent = box.prompt("Reply with exactly the word SDKOK and nothing else. Do not run any tools.", agent="codex")
        agent.wait()
        assert "SDKOK" in (agent.last_message or ""), (agent.last_message, agent.error)
        agent.send("Reply with exactly the word AGAINOK and nothing else.")
        agent.wait()
        assert "AGAINOK" in (agent.last_message or ""), agent.last_message
        assert any(e.get("type") == "end" for e in agent.events(follow=False))
        step("prompt (codex), send, events")
    else:
        print("skip agents: no openai key")
finally:
    for b in made:
        try:
            b.delete()
        except Exception:
            pass
    for s in saves:
        try:
            cx.delete_save(s)
        except Exception:
            pass
print("done")
