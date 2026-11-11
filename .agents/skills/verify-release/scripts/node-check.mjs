// Live checks of a sandbox on a node (v0.2.2 M3) for verify.sh's node area.
//   node node-check.mjs <api url> <sandbox id> <mode> [arg]   (key in CLOUDEX_API_KEY)
// modes:
//   resize        a terminal's resize reaches the shell (tput cols and lines)
//   tui           htop draws and quits, and vim writes a file, in a terminal
//   hangup MARK   a plain terminal runs sleep MARK; the socket closes without
//                 a goodbye (the caller then looks for leftovers)
//   rtt N         N trivial execs on one kept-alive connection: p50/p90/p95 ms
//   setup N       N ssh WebSockets (tcp/22): ms to the upgrade and to sshd's banner
//   ws PORT       a preview WebSocket: needs the link in arg 5; echoes through it
// Prints one line of JSON; exits 1 when the check fails.
const [, , api, box, mode, arg, arg2] = process.argv;
const key = process.env.CLOUDEX_API_KEY;
const base = `${api}/v1/sandboxes/${box}`;
const wsBase = base.replace(/^http/, "ws");
const done = (ok, detail) => {
  console.log(JSON.stringify({ ok, ...detail }));
  process.exit(ok ? 0 : 1);
};
setTimeout(() => done(false, { why: "timed out" }), mode === "rtt" || mode === "setup" ? 600_000 : 90_000);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const pct = (xs, p) => {
  const s = [...xs].sort((a, b) => a - b);
  return Math.round(s[Math.min(s.length - 1, Math.floor((p / 100) * s.length))]);
};

function shell(query) {
  const ws = new WebSocket(`${wsBase}/terminal?${query}`, { headers: { authorization: `Bearer ${key}` } });
  ws.binaryType = "arraybuffer";
  const s = { ws, text: "", exit: null, closed: false };
  ws.onmessage = (e) => {
    if (typeof e.data === "string") {
      const m = JSON.parse(e.data);
      if ("exit" in m) s.exit = m.exit;
    } else s.text += new TextDecoder().decode(e.data);
  };
  ws.onclose = () => (s.closed = true);
  s.open = new Promise((r, j) => ((ws.onopen = r), (ws.onerror = () => j(new Error("the socket didn't open")))));
  s.send = (t) => ws.send(new TextEncoder().encode(t));
  s.until = async (f, ms = 20_000) => {
    const end = Date.now() + ms;
    while (!f()) {
      if (Date.now() > end) throw new Error(`no answer; saw ${JSON.stringify(s.text.slice(-300))}`);
      await sleep(100);
    }
  };
  return s;
}

try {
  if (mode === "resize") {
    const s = shell("cols=80&rows=24");
    await s.open;
    s.send("echo A=$(tput cols)x$(tput lines)\r");
    await s.until(() => /A=\d+x\d+/.test(s.text));
    s.ws.send(JSON.stringify({ resize: [132, 43] }));
    await sleep(500);
    s.send("echo B=$(tput cols)x$(tput lines); exit\r");
    await s.until(() => s.closed);
    const a = /A=(\d+x\d+)/.exec(s.text)?.[1];
    const b = /B=(\d+x\d+)/.exec(s.text)?.[1];
    done(a === "80x24" && b === "132x43", { before: a, after: b });
  } else if (mode === "tui") {
    const s = shell("cols=120&rows=40");
    await s.open;
    s.send("htop -d 10\r");
    await s.until(() => /Load average|Tasks:/.test(s.text));
    const htop = true;
    s.send("q");
    await sleep(500);
    const at = s.text.length;
    s.send("vim -u NONE -N /tmp/vr-vim.txt\r");
    await s.until(() => s.text.length > at + 50);
    await sleep(500);
    s.send("ivim-was-here\x1b");
    await sleep(300);
    s.send(":wq\r");
    await sleep(800);
    s.send("echo C=$(cat /tmp/vr-vim.txt); rm -f /tmp/vr-vim.txt; exit\r");
    await s.until(() => s.closed);
    done(htop && /C=vim-was-here/.test(s.text), { htop, vim: /C=vim-was-here/.test(s.text) });
  } else if (mode === "hangup") {
    const s = shell("cols=80&rows=24");
    await s.open;
    s.send(`echo started; sleep ${arg}\r`);
    await s.until(() => /started/.test(s.text));
    await sleep(500);
    // No goodbye: the socket just goes, as a closed tab or a dropped line does.
    s.ws.close();
    await sleep(500);
    done(true, { closed: true });
  } else if (mode === "rtt") {
    const n = Number(arg) || 100;
    const ms = [];
    const server = [];
    for (let i = 0; i < n + 3; i++) {
      const t0 = performance.now();
      const r = await fetch(`${base}/exec`, {
        method: "POST",
        headers: { authorization: `Bearer ${key}`, "content-type": "application/json" },
        body: JSON.stringify({ cmd: "true", timeoutSec: 30 }),
      });
      const j = await r.json();
      const t = performance.now() - t0;
      if (j.exitCode !== 0) throw new Error(`exec answered ${JSON.stringify(j).slice(0, 200)}`);
      if (i >= 3) {
        ms.push(t);
        server.push(j.ms);
      }
    }
    done(true, { n, p50: pct(ms, 50), p90: pct(ms, 90), p95: pct(ms, 95), min: Math.round(Math.min(...ms)), max: Math.round(Math.max(...ms)), insideP50: pct(server, 50) });
  } else if (mode === "setup") {
    const n = Number(arg) || 20;
    const up = [];
    const banner = [];
    for (let i = 0; i < n; i++) {
      const t0 = performance.now();
      const ws = new WebSocket(`${wsBase}/tcp/22`, { headers: { authorization: `Bearer ${key}` } });
      ws.binaryType = "arraybuffer";
      let t1 = 0;
      await new Promise((resolve, reject) => {
        ws.onopen = () => (t1 = performance.now());
        ws.onmessage = (e) => {
          if (typeof e.data !== "string" && new TextDecoder().decode(e.data).startsWith("SSH-")) {
            up.push(t1 - t0);
            banner.push(performance.now() - t0);
            ws.close();
            resolve();
          }
        };
        ws.onerror = () => reject(new Error("the socket didn't open"));
      });
      await sleep(200);
    }
    done(true, { n, upgradeP50: pct(up, 50), upgradeP95: pct(up, 95), bannerP50: pct(banner, 50), bannerP95: pct(banner, 95) });
  } else if (mode === "ws") {
    const link = arg2;
    const ws = new WebSocket(link.replace(/^http/, "ws") + "/ws");
    const got = await new Promise((resolve, reject) => {
      ws.onopen = () => ws.send("ping-through-the-node");
      ws.onmessage = (e) => resolve(String(e.data));
      ws.onerror = () => reject(new Error("the preview WebSocket didn't open"));
    });
    ws.close();
    done(got === "echo:ping-through-the-node", { got });
  } else {
    done(false, { why: `unknown mode ${mode}` });
  }
} catch (e) {
  done(false, { why: e.message });
}
