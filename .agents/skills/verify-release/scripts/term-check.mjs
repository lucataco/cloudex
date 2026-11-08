// Live checks of the terminal WebSocket for verify.sh's dashboard area.
//   node term-check.mjs <api url> <sandbox id> <mode>   (key in CLOUDEX_API_KEY)
// modes:
//   echo    a shell answers, and its exit code comes back as {"exit":N}
//   tmux    a variable set in a tmux session is still there after reconnecting
//   dead    a TCP port nothing listens on is refused (502), not opened
// Prints one line of JSON; exits 1 when the check fails.
const [, , api, box, mode] = process.argv;
const key = process.env.CLOUDEX_API_KEY;
const base = api.replace(/^http/, "ws") + `/v1/sandboxes/${box}`;
const done = (ok, detail) => {
  console.log(JSON.stringify({ ok, ...detail }));
  process.exit(ok ? 0 : 1);
};
setTimeout(() => done(false, { why: "timed out" }), 60_000);

function shell(query) {
  const ws = new WebSocket(`${base}/terminal?${query}`, { headers: { authorization: `Bearer ${key}` } });
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
      if (Date.now() > end) throw new Error(`no answer; saw ${JSON.stringify(s.text.slice(-200))}`);
      await new Promise((r) => setTimeout(r, 100));
    }
  };
  return s;
}

try {
  if (mode === "echo") {
    const s = shell("cols=100&rows=30");
    await s.open;
    s.send("echo C=$(tput cols) W=$(whoami) S=$((6*7)); exit 7\r");
    await s.until(() => s.exit !== null && s.closed);
    const m = /C=(\d+) W=(\S+) S=42/.exec(s.text);
    done(!!m && m[1] === "100" && m[2] === "cloudex" && s.exit === 7, { cols: m?.[1], user: m?.[2], exit: s.exit });
  } else if (mode === "tmux") {
    const name = `vr-${Date.now() % 100000}`;
    const a = shell(`session=${name}`);
    await a.open;
    a.send("export VR_MARK=kept-$((40+2)); echo SET-$$\r");
    await a.until(() => /SET-\d+/.test(a.text));
    const pid = /SET-(\d+)/.exec(a.text)[1];
    a.ws.close();
    await new Promise((r) => setTimeout(r, 1000));
    const b = shell(`session=${name}`);
    await b.open;
    b.send("echo GOT-$VR_MARK-$$\r");
    await b.until(() => /GOT-\S+/.test(b.text.replace(/echo GOT/g, "")));
    const got = /GOT-(\S+)-(\d+)/.exec(b.text.replace(/echo GOT/g, ""));
    b.ws.send(JSON.stringify({ kill: 1 }));
    await b.until(() => b.exit !== null || b.closed, 10_000).catch(() => {});
    done(got?.[1] === "kept-42" && got?.[2] === pid, { mark: got?.[1], samePid: got?.[2] === pid, killed: b.exit !== null || b.closed });
  } else if (mode === "dead") {
    const ws = new WebSocket(`${base}/tcp/9`, { headers: { authorization: `Bearer ${key}` } });
    ws.onopen = () => done(false, { why: "a dead port opened" });
    ws.onerror = () => done(true, { refused: true });
  } else done(false, { why: `unknown mode ${mode}` });
} catch (e) {
  done(false, { why: e.message });
}
