// The dashboard checks, B01 onwards, as a user goes through them. Each
// scenario gets T from run.mjs: the signed-in main page, the run's sandbox,
// the CLI, and the bookkeeping that makes sure every session, token and
// sign-in it starts is ended again.
//
// Waits are for a condition, each with a timeout that says what it waited
// for. Values typed into the shell are computed by it ($((6*7)) and so on), so
// a match is the shell's output and never the echo of what was typed.
import fs from "node:fs";
import path from "node:path";
import { leakyUrls, poll } from "./util.mjs";

const must = (ok, why) => {
  if (!ok) throw new Error(why);
};
const secs = (t0) => `${((Date.now() - t0) / 1000).toFixed(1)} s`;

// ---------------------------------------------------------------- terminal helpers

// The visible terminal's rows, as text, matched in the page.
const termFind = (src, flags) => {
  const rows = [...(document.querySelector(".term:not([hidden]) .xterm-rows")?.children ?? [])];
  const text = rows.map((r) => r.textContent.replace(/ /g, " ").trimEnd()).join("\n");
  const m = new RegExp(src, flags).exec(text);
  return m ? [...m] : null;
};
const termLacks = (src, flags) => {
  const el = document.querySelector(".term:not([hidden]) .xterm-rows");
  return !!el && !new RegExp(src, flags).test(el.textContent);
};
const termRows = () => document.querySelector(".term:not([hidden]) .xterm-rows")?.children.length ?? 0;
const PROMPT = /[$#] ?$/m;

async function focusTerm(p) {
  await p.front();
  await p.waitFor(
    () => {
      const t = document.querySelector(".term:not([hidden]) textarea");
      if (!t) return false;
      t.focus();
      return document.activeElement === t;
    },
    [],
    { what: "the terminal to take the keyboard" },
  );
}
const waitTerm = (p, re, what, ms = 20_000) => p.waitFor(termFind, [re.source, re.flags], { ms, what });
async function sh(p, cmd, re, what, ms) {
  await focusTerm(p);
  await p.typeText(cmd);
  await p.press("Enter");
  return waitTerm(p, re, what ?? `the shell to answer ${JSON.stringify(cmd.slice(0, 40))}`, ms);
}
async function openTerminal(T, p) {
  await p.go(`${T.api}/s/${T.box.name}/terminal`);
  await p.front();
  await waitTerm(p, PROMPT, "a shell prompt in the terminal tab", 60_000);
}

// ---------------------------------------------------------------- signin

export async function signin(T) {
  const S = T.scenario("signin");
  const p = T.main;
  let first;
  await S.check("B01", "cloudex dashboard's one-time link signs the browser in on the sandbox list, and its code leaves the address bar", async () => {
    first = await T.link();
    await p.go(first);
    await p.waitFor(() => location.pathname === "/" && !!document.querySelector("header.top .who") && document.querySelector("h1")?.textContent === "Sandboxes", [], {
      ms: 30_000,
      what: "the sandbox list, signed in",
    });
    const me = await T.me(p);
    must(me?.token?.kind === "session", "/v1/me doesn't show a dashboard session");
    must(!(await p.ev(() => location.href.includes("link="))), "the link's code is still in the address bar");
    T.mainSession = me.token.id;
  });
  if (!T.mainSession) return false;
  await S.check("B02", "the sandbox list shows the run's sandbox", () =>
    p.waitFor((n) => [...document.querySelectorAll("table.list tbody td:first-child a")].some((a) => a.textContent === n), [T.box.name], {
      ms: 20_000,
      what: `a row for ${T.box.name}`,
    }),
  );
  await S.check(
    "B03",
    "the used link, opened again in a private window, says it was used and signs nothing in",
    async () => {
      const { ctx, page } = await T.newContext();
      try {
        await page.go(first);
        await page.waitFor(() => /used already|expired/.test(document.querySelector(".problem")?.textContent ?? ""), [], { ms: 20_000, what: "the login page to say the link was used" });
        must(!(await T.me(page)), "the second use signed the window in");
      } finally {
        await T.endContext(ctx);
      }
    },
    { alone: true },
  );
  await S.check(
    "B04",
    "a new link opened in a browser that's signed in keeps its session",
    async () => {
      const again = await T.link();
      await p.go(again);
      await p.waitFor(
        () =>
          document.readyState === "complete" &&
          !document.body.innerText.includes("Signing in…") &&
          (location.pathname !== "/login" || !!document.querySelector("main.login, .problem, .note")),
        [],
        { ms: 20_000, what: "the page to settle after opening the link" },
      );
      const me = await T.me(p);
      must(!!me, "the browser isn't signed in any more");
      must(me.token?.id === T.mainSession, "the link was traded for a new session");
      must(!(await p.ev(() => location.href.includes("link="))), "the link's code is still in the address bar");
    },
    { alone: true },
  );
  return true;
}

// ---------------------------------------------------------------- terminal

export async function terminal(T) {
  const S = T.scenario("terminal");
  const p = T.main;
  await S.check("B05", "the terminal tab gets a shell as cloudex", async () => {
    await openTerminal(T, p);
    const m = await sh(p, "echo W=$(whoami) S=$((6*7))", /^W=(\S+) S=42$/m);
    must(m[1] === "cloudex", `the shell runs as ${m[1]}`);
  });
  await S.check("B06", "tput sees the terminal's size, and follows the window when it's resized", async () => {
    const rows = await p.ev(termRows);
    const a = await sh(p, "echo C1=$(tput cols)x$(tput lines)", /^C1=(\d+)x(\d+)$/m);
    must(Number(a[2]) === rows, `tput lines says ${a[2]}, the page shows ${rows} rows`);
    await p.size(900, 600);
    try {
      const fewer = await p.waitFor((n) => document.querySelector(".term:not([hidden]) .xterm-rows")?.children.length < n, [rows], { what: "the terminal to get fewer rows" });
      must(fewer, "no resize");
      let n = 0;
      // The new size reaches the shell over the socket: ask until it has.
      await poll(
        async () => {
          const tag = `C2${String.fromCharCode(97 + (n++ % 26))}`;
          const b = await sh(p, `echo ${tag}=$(tput cols)x$(tput lines)`, new RegExp(`^${tag}=(\\d+)x(\\d+)$`, "m"), `tput's answer ${tag}`, 5_000);
          return Number(b[1]) < Number(a[1]) && Number(b[2]) === (await p.ev(termRows));
        },
        { ms: 20_000, every: 500, what: "tput to show the smaller terminal" },
      );
    } finally {
      await p.size(1300, 820);
      await p.waitFor((n) => document.querySelector(".term:not([hidden]) .xterm-rows")?.children.length === n, [rows], { what: "the terminal to grow back" }).catch(() => {});
    }
  });
  await S.check(
    "B07",
    "vi edits a file and :wq saves it",
    async () => {
      try {
        await sh(p, "vi /tmp/vr-vi.txt", /^~/m, "vi to open");
        await p.typeText("ihello from vi");
        await waitTerm(p, /hello from vi/, "the typed text in vi");
        await p.press("Escape");
        await p.typeText(":wq");
        await waitTerm(p, /^:wq/m, ":wq on vi's command line");
        await p.press("Enter");
        await sh(p, "echo V=$(tr a-z A-Z </tmp/vr-vi.txt)", /^V=HELLO FROM VI$/m, "the file vi saved");
      } catch (e) {
        // Leave vi, so the next checks get the shell.
        await p.press("Escape").catch(() => {});
        await p.typeText(":q!").catch(() => {});
        await p.press("Enter").catch(() => {});
        throw e;
      }
    },
    { alone: true },
  );
  await S.check(
    "B08",
    "htop draws its screen and quits with q",
    async () => {
      await sh(p, "htop", /Load average|Tasks:/, "htop's screen");
      await p.typeText("q");
      await p.waitFor(termLacks, ["Load average", ""], { what: "htop to quit" });
      await sh(p, "echo H=$((2+3))", /^H=5$/m, "the shell after htop");
    },
    { alone: true },
  );
  await S.check("B09", "UTF-8 typed in and printed out shows as itself", () => sh(p, "printf 'U=%s\\n' héllo-世界", /^U=héllo-世界$/m, "héllo-世界 printed back"), { alone: true });
  await S.check("B10", "+ shell opens a second shell, and + root a root one", async () => {
    await p.click("+ shell");
    await p.waitFor(() => document.querySelectorAll(".term-tab").length === 2 && document.querySelector(".term-tab.on button")?.textContent.trim() === "shell 2", [], { what: "a second tab, shown" });
    await waitTerm(p, PROMPT, "a prompt in shell 2", 30_000);
    await sh(p, "echo T2=$(whoami)-$((1+1))", /^T2=cloudex-2$/m, "shell 2 as cloudex");
    await p.click("+ root");
    await p.waitFor(() => document.querySelectorAll(".term-tab").length === 3 && document.querySelector(".term-tab.on button")?.textContent.trim() === "root 3", [], { what: "a root tab, shown" });
    await waitTerm(p, PROMPT, "a prompt in the root tab", 30_000);
    await sh(p, "echo T3=$(whoami)-$((1+2))", /^T3=root-3$/m, "the root tab as root");
  });
  await S.check("B11", "after a reload the same shells come back, with what was on them", async () => {
    await p.click("shell 1", { selector: ".term-tab button" });
    await p.waitFor(() => document.querySelector(".term-tab.on button")?.textContent.trim() === "shell 1", [], { what: "shell 1 to show" });
    const before = await sh(p, "echo M1=$((1000+1)) P1=$$", /^M1=1001 P1=(\d+)$/m);
    await p.reload();
    await p.waitFor(() => document.querySelectorAll(".term-tab").length === 3, [], { ms: 30_000, what: "the three tabs back after the reload" });
    await p.front();
    await waitTerm(p, /^M1=1001 P1=\d+$/m, "shell 1's earlier output back on its screen", 30_000);
    const after = await sh(p, "echo Q1=$$", /^Q1=(\d+)$/m);
    must(after[1] === before[1], `the shell is a new one ($$ was ${before[1]}, now ${after[1]})`);
  });
  // Clicks the x on the tab named name.
  const closeTab = (name) =>
    p.waitFor(
      (n) => {
        const t = [...document.querySelectorAll(".term-tab")].find((s) => s.querySelector("button")?.textContent.trim() === n);
        if (!t) return false;
        t.querySelector("button.x").click();
        return true;
      },
      [name],
      { what: `${name}'s close button` },
    );
  // Waits until the dashboard's tmux sessions (root's with root) are those that ok() likes.
  const tmuxUntil = async (root, ok, what) => {
    let last = "";
    await poll(
      async () => {
        const r = await T.cx(["exec", ...(root ? ["--root"] : []), T.box.id, "--", "tmux -L cloudex-dash ls -F '#S' 2>&1"]);
        last = r.out.trim().split("\n").join(" ");
        return ok(r.out.trim().split("\n"));
      },
      { ms: 20_000, every: 1_000, what },
    ).catch((e) => {
      throw new Error(`${e.message} (it lists: ${last || "nothing"})`);
    });
  };
  await S.check("B12", "closing a tab ends its tmux session", async () => {
    await closeTab("shell 2");
    await tmuxUntil(false, (l) => l.includes("dash-1") && !l.includes("dash-2"), "tmux -L cloudex-dash ls to list dash-1 and not dash-2");
  });
  // Right after a reload a tab's socket can still be opening (Firefox opens
  // one at a time to a host): closing the tab then must end its session too.
  await S.check("B12b", "a tab closed while its socket is still opening, just after a reload, ends its tmux session", async () => {
    await p.reload();
    await closeTab("root 2");
    await p.waitFor(() => document.querySelectorAll(".term-tab").length === 1, [], { what: "one tab left" });
    await tmuxUntil(true, (l) => !l.includes("dash-3-root"), "root's tmux -L cloudex-dash ls not to list dash-3-root");
  });
}

// ---------------------------------------------------------------- reconnect

export async function reconnect(T) {
  const S = T.scenario("reconnect");
  const p = T.main;
  await S.check("B13", "a terminal whose socket drops shows reconnecting… and gets the same shell back", async () => {
    if (!(await p.ev(() => location.pathname.endsWith("/terminal")))) await openTerminal(T, p);
    const before = await sh(p, "export DROP=yes-$((1+1)); echo RB=$$", /^RB=(\d+)$/m);
    // Offline emulation leaves an open WebSocket alone: close it from the page,
    // as a deploy does from the server.
    const dropped = await p.ev(() => {
      window.__vrSaw = false;
      window.__vrBefore = window.__vrWs.length;
      new MutationObserver(() => {
        if (document.querySelector(".term:not([hidden]) .term-status")?.textContent.includes("reconnecting")) window.__vrSaw = true;
      }).observe(document.body, { subtree: true, childList: true, characterData: true });
      const open = window.__vrWs.filter((w) => w.readyState === 1 && w.url.includes("/terminal"));
      open.forEach((w) => w.close(4000, "verify drop"));
      return open.length;
    });
    must(dropped > 0, "no open terminal socket to drop");
    const t0 = Date.now();
    await p.waitFor(
      () =>
        window.__vrSaw &&
        window.__vrWs.length > window.__vrBefore &&
        document.querySelector(".term:not([hidden]) .term-status")?.textContent === "",
      [],
      { ms: 30_000, what: "reconnecting… and then a new open socket" },
    );
    const after = await sh(p, "echo RA=$$ D=$DROP", /^RA=(\d+) D=(\S*)$/m, "the shell after reconnecting");
    must(after[1] === before[1] && after[2] === "yes-2", `a different shell came back ($$ ${before[1]} → ${after[1]}, DROP=${after[2]})`);
    return secs(t0);
  });
}

// ---------------------------------------------------------------- desktop

export async function desktop(T) {
  const S = T.scenario("desktop");
  const p = T.main;
  const canvas = () => {
    const c = document.querySelector(".vnc canvas");
    return c && c.width > 0 ? `${c.width}x${c.height}` : null;
  };
  await S.check("B14", "the desktop tab starts the desktop and shows its screen", async () => {
    await p.go(`${T.api}/s/${T.box.name}/desktop`);
    await p.front();
    // A start that fails shows why on the page: say so at once rather than wait out the time.
    const r = await p.waitFor(
      () =>
        [...document.querySelectorAll(".toolbar button")].some((b) => b.textContent.trim() === "Stop the desktop")
          ? "up"
          : /failed/.test(document.querySelector(".problem")?.textContent ?? "") && document.querySelector(".problem").textContent.trim().slice(0, 200),
      [],
      { ms: 300_000, every: 1_000, what: "the desktop to start and connect (the first start installs it)" },
    );
    must(r === "up", `the page says: ${r}`);
    await p.waitFor(canvas, [], { ms: 30_000, what: "noVNC's canvas to draw" });
  });
  await S.check("B15", "the size menu resizes the desktop", async () => {
    const now = await p.ev(() => document.querySelector('select[aria-label="Screen size"]')?.value ?? "");
    const want = now === "1280x720" ? "1440x900" : "1280x720";
    await p.setValue('select[aria-label="Screen size"]', want);
    await p.waitFor((w) => {
      const c = document.querySelector(".vnc canvas");
      return !!c && `${c.width}x${c.height}` === w;
    }, [want], { ms: 90_000, every: 500, what: `the desktop to be ${want}` });
  });
  // Not a check: the desktop needn't run on while the rest goes on.
  await p.click("Stop the desktop", { ms: 3_000 }).catch(() => {});
}

// ---------------------------------------------------------------- agent

export async function agent(T) {
  const S = T.scenario("agent");
  const p = T.main;
  const ids = ["B16", "B17", "B18"];
  const what = [
    "an OpenCode turn shows the command, its output, the reply and turn 1 done",
    "a second message continues the conversation",
    "Interrupt stops a long turn",
  ];
  if (!T.agents.on) return ids.forEach((id, i) => S.skip(id, what[i], T.agents.why));
  const log = () => [...document.querySelectorAll(".log .l")].map((l) => ({ kind: l.className.replace(/^l l-/, "").split(" ")[0], text: l.textContent }));
  const ended = (n) => [...document.querySelectorAll(".l-end, .l-error")].map((e) => e.textContent).find((t) => new RegExp(`turn ${n} (done|failed|interrupted)`).test(t)) ?? null;
  await S.check("B16", what[0], async () => {
    await p.go(`${T.api}/s/${T.box.name}/agents`);
    await p.click("New agent");
    await p.setValue("section.card select", "opencode");
    await p.setValue('section.card input[placeholder="provider/model"]', "anthropic/claude-haiku-4-5");
    await p.setValue("section.card textarea", "Run the shell command `uname -s`, then reply with just the word Ready.");
    await p.click("Start");
    await p.waitFor(() => /\/agents\/ag-/.test(location.pathname), [], { ms: 30_000, what: "the new agent's page" });
    const end = await p.waitFor(ended, [1], { ms: 180_000, every: 1_000, what: "turn 1 to end" });
    const lines = await p.ev(log);
    must(/turn 1 done/.test(end), `it ended: ${end}`);
    must(lines.some((l) => l.kind === "cmd" && l.text.includes("uname")), "no command line with uname");
    must(lines.some((l) => l.text.includes("Linux")), "no Linux in the command's output");
    must(lines.some((l) => l.kind === "text" && /Ready/.test(l.text)), "no reply with Ready");
  });
  await S.check("B17", what[1], async () => {
    await p.setValue(".send textarea", "Reply with just the word Again.");
    await p.click("Send");
    const end = await p.waitFor(ended, [2], { ms: 180_000, every: 1_000, what: "turn 2 to end" });
    must(/turn 2 done/.test(end), `it ended: ${end}`);
    must((await p.ev(log)).some((l) => l.kind === "text" && /Again/.test(l.text)), "no reply with Again");
  });
  await S.check("B18", what[2], async () => {
    // In the foreground: OpenCode may otherwise put it in the background and end the turn at once.
    await p.setValue(".send textarea", "Run the shell command `sleep 300` in the foreground and wait for it to finish (do not run it in the background), then reply with just the word Done.");
    await p.click("Send");
    // A command's line may only show once it ends, so give the turn time to start it instead of waiting for the line.
    await new Promise((r) => setTimeout(r, 25_000));
    const early = await p.ev(ended, 3);
    must(!early, `the agent ended turn 3 itself before Interrupt (${early}): it didn't wait for sleep 300`);
    await p.click("Interrupt");
    const end = await p.waitFor(ended, [3], { ms: 90_000, every: 1_000, what: "turn 3 to end" });
    must(/turn 3 interrupted/.test(end), `it ended: ${end}`);
  });
}

// ---------------------------------------------------------------- work: commands, files, saves, access

export async function work(T) {
  const S = T.scenario("work");
  const p = T.main;
  const HTML_NAME = "<img src=x onerror=window.__vrx=1>.html";
  const HTML_BODY = `<script>window.__vrx=2</script>vr-html-${T.prefix}`;
  const picked = path.join(T.work, "vr-picked.txt");
  fs.writeFileSync(picked, `picked-${T.prefix}\n`);
  const yes = () => void (window.confirm = () => true);

  await S.check(
    "B19",
    "a background command shows its output and exit code, and Remove deletes it",
    async () => {
      await p.go(`${T.api}/s/${T.box.name}/commands`);
      await p.setValue("textarea.mono", "for i in 1 2 3; do echo tick-$i; sleep 1; done; exit 3");
      await p.click("Start");
      await p.waitFor(() => /\/commands\/cm-/.test(location.pathname), [], { ms: 30_000, what: "the command's page" });
      await p.waitFor(() => document.querySelector("pre.output")?.textContent.includes("tick-3") && document.querySelector(".head.sub")?.textContent.includes("exit 3"), [], {
        ms: 60_000,
        what: "tick-3 and exit 3",
      });
      await p.ev(yes);
      await p.click("Remove");
      await p.waitFor(() => /\/commands$/.test(location.pathname), [], { what: "the list of commands after Remove" });
    },
    { alone: true },
  );

  await S.check("B20", "files upload with the picker and by dropping them", async () => {
    const r = await T.cx(["exec", T.box.id, "--", "mkdir -p ~/vr-dir && echo in-dir >~/vr-dir/a.txt"]);
    must(r.code === 0, `couldn't make ~/vr-dir: ${r.err}`);
    await p.go(`${T.api}/s/${T.box.name}/files`);
    await p.waitFor(() => !!document.querySelector("table.list") && !!document.querySelector('input[type="file"]'), [], { ms: 30_000, what: "the home directory's listing" });
    await p.setFiles('input[type="file"]', [picked]);
    await p.waitFor(() => [...document.querySelectorAll("table.list a")].some((a) => a.textContent === "vr-picked.txt"), [], { ms: 30_000, what: "vr-picked.txt in the list" });
    const dropped = await p.ev(
      (name, body) => {
        const dt = new DataTransfer();
        dt.items.add(new File([body], name, { type: "text/html" }));
        const zone = document.querySelector("section.files");
        if (!zone) return false;
        zone.dispatchEvent(new DragEvent("dragover", { dataTransfer: dt, bubbles: true, cancelable: true }));
        zone.dispatchEvent(new DragEvent("drop", { dataTransfer: dt, bubbles: true, cancelable: true }));
        return true;
      },
      HTML_NAME,
      HTML_BODY,
    );
    must(dropped, "no drop zone");
    await p.waitFor((n) => [...document.querySelectorAll("table.list a")].some((a) => a.textContent === n), [HTML_NAME], { ms: 30_000, what: "the dropped file in the list" });
  });

  await S.check("B21", "a file named like HTML shows as that text", () =>
    p.ev((n) => {
      const a = [...document.querySelectorAll("table.list a")].find((x) => x.textContent === n);
      if (!a) throw new Error("the file isn't listed by its name");
      if (document.querySelector("table.list img")) throw new Error("its name became an element");
      if (window.__vrx) throw new Error("its name ran as script");
      return true;
    }, HTML_NAME),
  );

  const newDownload = async (before, test, what) =>
    poll(
      () => {
        for (const f of fs.readdirSync(T.dl)) {
          if (before.has(f) || /\.(crdownload|part|tmp)$/.test(f)) continue;
          const buf = fs.readFileSync(path.join(T.dl, f));
          if (test(f, buf)) return f;
        }
        return null;
      },
      { ms: 60_000, every: 300, what },
    );

  await S.check(
    "B22",
    "downloading that file saves it rather than opening it",
    async () => {
      const before = new Set(fs.readdirSync(T.dl));
      const here = await p.ev(() => location.href);
      const clicked = await p.ev((n) => {
        const a = [...document.querySelectorAll("table.list a")].find((x) => x.textContent === n);
        a?.click();
        return !!a;
      }, HTML_NAME);
      must(clicked, "the file isn't listed");
      await newDownload(before, (_, buf) => buf.toString("utf8") === HTML_BODY, "the file in the downloads directory");
      must((await p.ev(() => location.href)) === here, "the page went somewhere else");
      must(!(await p.ev(() => window.__vrx)), "the file's script ran");
    },
    { alone: true },
  );

  await S.check(
    "B23",
    "a directory downloads as tar.gz",
    async () => {
      const before = new Set(fs.readdirSync(T.dl));
      const clicked = await p.ev(() => {
        const row = [...document.querySelectorAll("table.list tr")].find((tr) => tr.querySelector("button")?.textContent === "vr-dir/");
        const a = row && [...row.querySelectorAll("a")].find((x) => x.textContent.trim() === "tar.gz");
        a?.click();
        return !!a;
      });
      must(clicked, "no tar.gz button on vr-dir's row");
      await newDownload(before, (_, buf) => buf[0] === 0x1f && buf[1] === 0x8b, "a gzip file in the downloads directory");
    },
    { alone: true },
  );

  await S.check(
    "B24",
    "Delete removes a file",
    async () => {
      await p.ev(yes);
      const clicked = await p.ev(() => {
        const row = [...document.querySelectorAll("table.list tr")].find((tr) => tr.querySelector("a")?.textContent === "vr-picked.txt");
        const b = row && [...row.querySelectorAll("button")].find((x) => x.textContent.trim() === "Delete");
        b?.click();
        return !!b;
      });
      must(clicked, "no Delete button on vr-picked.txt's row");
      await p.waitFor(() => ![...document.querySelectorAll("table.list a")].some((a) => a.textContent === "vr-picked.txt"), [], { ms: 20_000, what: "vr-picked.txt to leave the list" });
    },
    { alone: true },
  );

  await S.check(
    "B25",
    "the Saves page loads",
    async () => {
      await p.go(`${T.api}/saves`);
      await p.waitFor(() => document.querySelector("h1")?.textContent === "Saves" && !document.body.innerText.includes("Loading"), [], { ms: 20_000, what: "the saves, loaded" });
    },
    { alone: true },
  );

  if (!T.admin) S.skip("B26", "the Access page lists this browser's session", "this config isn't an admin, so it has no Access page");
  else
    await S.check(
      "B26",
      "the Access page lists this browser's session",
      async () => {
        await p.go(`${T.api}/access`);
        await p.waitFor(() => document.body.innerText.includes("AI provider keys") && !document.body.innerText.includes("Loading"), [], { ms: 20_000, what: "the Access page, loaded" });
        must((await p.text()).includes("this browser"), "no session is marked this browser");
      },
      { alone: true },
    );
}

// ---------------------------------------------------------------- code: other ways in, and approving

export async function code(T) {
  const S = T.scenario("code");
  const box = `/s/${T.box.name}`;
  const noAdmin = "needs an admin config (it approves and revokes with cloudex tokens)";

  {
    const { ctx, page } = await T.newContext();
    S.page = page;
    try {
      await S.check("B27", "signed out, a sandbox's page goes to the login page, which keeps where it was going", async () => {
        await page.go(`${T.api}${box}`);
        await page.waitFor(() => location.pathname === "/login" && !!document.querySelector("main.login"), [], { ms: 20_000, what: "the login page" });
        const next = await page.ev(() => new URLSearchParams(location.search).get("next"));
        must(next === box, `next is ${next}`);
      });
      if (!T.admin) S.skip("B28", "Get a code, approved with cloudex tokens approve, signs in and lands there", noAdmin);
      else
        await S.check("B28", "Get a code, approved with cloudex tokens approve, signs in and lands there", async () => {
          await page.click("Get a code");
          const userCode = await page.waitFor(() => document.querySelector(".code")?.textContent.trim() || null, [], { ms: 20_000, what: "a code on the login page" });
          T.state.codes.add(userCode);
          const r = await T.cx(["tokens", "approve", userCode, "--yes", "--role", "user", "--expires", "1h", "--json"]);
          must(r.code === 0, `tokens approve failed: ${r.err}`);
          T.state.codes.delete(userCode);
          await page.waitFor((b) => location.pathname === b, [box], { ms: 30_000, what: `the page to sign in and go to ${box}` });
          await page.waitFor(() => document.querySelector(".who span")?.textContent.includes("· user"), [], { what: "the header to show a user session" });
          must((await T.me(page))?.role === "user", "the session isn't a user one");
        });
    } finally {
      await T.endContext(ctx);
    }
  }

  if (!T.admin) {
    for (const [id, d] of [
      ["B29", "a pasted read token signs in read-only: no New sandbox, Terminal or Desktop"],
      ["B30", "revoking the token ends the session it signed in"],
      ["B31", "/login?code= shows the sign-in's facts and starts its role at user, even when admin was asked"],
      ["B32", "Approve there signs the other computer in, as user"],
    ])
      S.skip(id, d, noAdmin);
    return;
  }

  {
    const { ctx, page } = await T.newContext();
    S.page = page;
    let tokenId;
    try {
      await S.check("B29", "a pasted read token signs in read-only: no New sandbox, Terminal or Desktop", async () => {
        const r = await T.cx(["tokens", "create", `${T.prefix}-read`, "--role", "read", "--expires", "1h", "--json"]);
        must(r.code === 0 && r.json?.secret, `tokens create failed: ${r.err}`);
        tokenId = r.json.id;
        T.secret(r.json.secret);
        await page.go(`${T.api}/login`);
        await page.setValue('input[type="password"]', r.json.secret);
        await page.click("Sign in", { selector: "button" });
        await page.waitFor(() => location.pathname === "/" && document.querySelector(".who span")?.textContent.includes("· read"), [], { ms: 20_000, what: "the list, signed in as read" });
        must((await T.me(page))?.role === "read", "the session isn't a read one");
        must(!(await page.ev(() => [...document.querySelectorAll("button")].some((b) => b.textContent.trim() === "New sandbox"))), "it shows New sandbox");
        await page.go(`${T.api}${box}`);
        const tabs = await page.waitFor(() => (document.querySelector("nav.tabs") ? [...document.querySelectorAll("nav.tabs a")].map((a) => a.textContent.trim()) : null), [], { ms: 20_000, what: "the sandbox's tabs" });
        must(tabs.includes("Overview") && !tabs.includes("Terminal") && !tabs.includes("Desktop"), `the tabs are ${tabs.join(", ")}`);
      });
      await S.check("B30", "revoking the token ends the session it signed in", async () => {
        must(tokenId, "no token");
        const r = await T.cx(["tokens", "revoke", tokenId]);
        must(r.code === 0, `tokens revoke failed: ${r.err}`);
        const t0 = Date.now();
        await page.waitFor(() => location.pathname === "/login", [], { ms: 30_000, every: 500, what: "the page to go to the login page" });
        return secs(t0);
      });
    } finally {
      await T.endContext(ctx);
    }
  }

  const p = T.main;
  S.page = p;
  let login;
  await S.check("B31", "/login?code= shows the sign-in's facts and starts its role at user, even when admin was asked", async () => {
    const dir = fs.mkdtempSync(path.join(T.work, "login-"));
    login = T.spawnCx(["login", "--url", T.api, "--name", `${T.prefix}-approve`, "--role", "admin", "--json"], {
      CLOUDEX_CONFIG_DIR: dir,
      CLOUDEX_API_KEY: "",
      CLOUDEX_API_URL: "",
    });
    login.dir = dir;
    const first = await poll(() => {
      if (login.exited) throw Object.assign(new Error(`cloudex login exited: ${login.err}`), { fatal: true });
      const line = login.out.split("\n")[0];
      try {
        return JSON.parse(line).userCode;
      } catch {
        return null;
      }
    }, { ms: 30_000, what: "cloudex login to print its code" });
    T.state.codes.add(first);
    login.code = first;
    await p.go(`${T.api}/login?code=${encodeURIComponent(first)}`);
    await p.waitFor(() => document.querySelector("h1")?.textContent === "Approve a sign-in", [], { ms: 20_000, what: "the Approve a sign-in page" });
    const facts = await p.ev(() => document.querySelector(".facts")?.innerText ?? "");
    must(facts.includes(`${T.prefix}-approve`), "it doesn't show the name the computer gave");
    must(facts.includes(first), "it doesn't show the code");
    const role = await p.ev(() => document.querySelector(".form select")?.value);
    must(role === "user", `the role starts at ${role}`);
  });
  await S.check("B32", "Approve there signs the other computer in, as user", async () => {
    must(login?.code, "no sign-in to approve");
    await p.click("Approve");
    await p.waitFor(() => document.body.innerText.includes("Approved."), [], { ms: 20_000, what: "Approved." });
    T.state.codes.delete(login.code);
    await poll(() => login.exited, { ms: 30_000, what: "cloudex login to finish" });
    must(login.status === 0, `cloudex login exited ${login.status}: ${login.err}`);
    const st = await T.cx(["status", "--json"], { CLOUDEX_CONFIG_DIR: login.dir, CLOUDEX_API_KEY: "", CLOUDEX_API_URL: "" });
    must(st.json?.me?.token?.name === `${T.prefix}-approve`, "the other computer isn't signed in with its own token");
    must(st.json?.me?.role === "user", `it's signed in as ${st.json?.me?.role}`);
  });
}

// ---------------------------------------------------------------- next

export async function next(T) {
  const S = T.scenario("next");
  const p = T.main;
  const box = `/s/${T.box.name}`;
  await S.check("B33", "next on the login page goes only to dashboard routes", async () => {
    const cases = [
      ["/v1/me", "/"],
      ["/v1/sandboxes", "/"],
      ["//example.com/", "/"],
      ["https://example.com/", "/"],
      ["/saves", "/saves"],
      [`${box}/files`, `${box}/files`],
    ];
    const wrong = [];
    for (const [n, want] of cases) {
      await p.go(`${T.api}/login?next=${encodeURIComponent(n)}`);
      await p.waitFor(() => location.pathname !== "/login" && document.readyState !== "loading", [], { ms: 20_000, what: `the login page to move on from next=${n}` });
      const got = await p.ev((api) => (location.origin === api ? location.pathname + location.search : location.href), T.api);
      if (got !== want) wrong.push(`next=${n} went to ${got}, not ${want}`);
    }
    must(!wrong.length, wrong.join("; "));
  });
}

// ---------------------------------------------------------------- phone

export async function phone(T) {
  const S = T.scenario("phone");
  const p = T.main;
  await S.check("B34", "at phone width the list and a sandbox's page don't scroll sideways", async () => {
    await p.size(390, 800);
    try {
      const wide = [];
      const measure = () => (document.documentElement.scrollWidth > innerWidth + 1 ? `${document.documentElement.scrollWidth}px in ${innerWidth}px` : null);
      await p.go(`${T.api}/`);
      await p.waitFor(() => !!document.querySelector("table.list"), [], { ms: 20_000, what: "the list" });
      const a = await p.ev(measure);
      if (a) wide.push(`the list is ${a}`);
      await p.go(`${T.api}/s/${T.box.name}`);
      await p.waitFor(() => !!document.querySelector(".facts"), [], { ms: 20_000, what: "the sandbox's overview" });
      const b = await p.ev(measure);
      if (b) wide.push(`the sandbox's page is ${b}`);
      must(!wide.length, wide.join("; "));
    } finally {
      await p.size(1300, 820);
    }
  });
}

// ---------------------------------------------------------------- revoke: sign out, revoke, the server ending a terminal

export async function revoke(T) {
  const S = T.scenario("revoke");
  {
    const { ctx, page } = await T.newContext();
    S.page = page;
    try {
      await S.check(
        "B35",
        "Sign out ends the session, and the next page is the login page",
        async () => {
          await T.signIn(page);
          await page.click("Sign out");
          await page.waitFor(() => location.pathname === "/login", [], { ms: 20_000, what: "the login page after Sign out" });
          await page.go(`${T.api}/s/${T.box.name}`);
          await page.waitFor(() => location.pathname === "/login", [], { ms: 20_000, what: "the login page again" });
          must(!(await T.me(page)), "the session still works");
        },
        { alone: true },
      );
    } finally {
      await T.endContext(ctx);
    }
  }
  if (!T.admin) {
    S.skip("B36", "revoking a session sends its open page to the login page", "needs an admin config (cloudex tokens revoke)");
    S.skip("B37", "the server closes a revoked session's open terminal within 90 s", "needs an admin config (cloudex tokens revoke)");
    return;
  }
  const { ctx, page } = await T.newContext();
  S.page = page;
  try {
    let sid, quiet, t0;
    await S.check("B36", "revoking a session sends its open page to the login page", async () => {
      sid = (await T.signIn(page)).token.id;
      await openTerminal(T, page);
      // A second tab of the same session, on a page that doesn't poll, holds a
      // terminal socket of its own: only the server can end that one.
      quiet = await T.browser.newPage(ctx);
      await quiet.go(`${T.api}/saves`);
      await quiet.waitFor(() => document.querySelector("h1")?.textContent === "Saves", [], { ms: 20_000, what: "the Saves page in the second tab" });
      const opened = await quiet.ev(
        (id) =>
          new Promise((resolve) => {
            const r = (window.__vrRaw = { got: 0, closed: null });
            const ws = new WebSocket(`${location.protocol === "http:" ? "ws" : "wss"}://${location.host}/v1/sandboxes/${id}/terminal?cols=80&rows=24`);
            ws.binaryType = "arraybuffer";
            ws.onopen = () => resolve(true);
            ws.onmessage = () => r.got++;
            ws.onclose = (e) => {
              r.closed = { code: e.code, at: Date.now() };
              resolve(false);
            };
            window.__vrRawWs = ws;
          }),
        T.box.id,
      );
      must(opened, "the second tab's terminal socket didn't open");
      await quiet.waitFor(() => window.__vrRaw.got > 0, [], { ms: 20_000, what: "the second tab's shell to send something" });
      const r = await T.cx(["tokens", "revoke", sid]);
      must(r.code === 0, `tokens revoke failed: ${r.err}`);
      t0 = Date.now();
      await page.front();
      await page.waitFor(() => location.pathname === "/login", [], { ms: 30_000, every: 500, what: "the terminal page to go to the login page" });
      return secs(t0);
    });
    await S.check("B37", "the server closes a revoked session's open terminal within 90 s", async () => {
      must(quiet && t0, "no open terminal to watch");
      S.page = quiet;
      await quiet.front();
      const closed = await quiet.waitFor(() => window.__vrRaw?.closed, [], { ms: Math.max(1_000, 90_000 - (Date.now() - t0)), every: 1_000, what: "the server to close the revoked session's terminal" });
      // 4401 is the server's "signed out", which the dashboard doesn't reconnect after.
      must(closed.code === 4401, `it closed with ${closed.code}, not 4401`);
      return secs(t0);
    });
  } finally {
    await T.endContext(ctx);
  }
}

// ---------------------------------------------------------------- console

export async function consoleCheck(T) {
  const S = T.scenario("console");
  const pages = [...T.browser.pages];
  const logs = pages.flatMap((p) => p.logs);
  await S.check(
    "B38",
    "no CSP violations or uncaught script errors in any page",
    () => {
      const csp = logs.filter((l) => /\[csp\]|Content.Security.Policy/i.test(l.text));
      const errors = logs.filter((l) => l.kind === "exception");
      must(!csp.length, `${csp.length} CSP line(s), first: ${csp[0]?.text}`);
      must(!errors.length, `${errors.length} uncaught error(s), first: ${errors[0]?.text}`);
    },
    { alone: true },
  );
  await S.check(
    "B39",
    "no request URL carries a token, secret or sign-in code",
    () => {
      const urls = pages.flatMap((p) => p.urls);
      const bad = leakyUrls(urls, T.secrets);
      must(urls.length > 0, "no requests were seen");
      must(!bad.length, `${bad.length} request(s), first to ${String(bad[0]).split("?")[0].replace(/^\w+:\/\/[^/]+/, "")}`);
    },
    { alone: true },
  );
}

// ---------------------------------------------------------------- nodes (v0.2.2)

export async function nodes(T) {
  const S = T.scenario("nodes");
  const p = T.main;
  const what = [
    "the sandbox list and the sandbox's page say it runs on the node, and its isolation",
    "the Nodes page lists the node, online, with its name as text",
  ];
  if (!T.node) {
    S.skip("B41", what[0], "the run's sandbox is on Cloudflare: set VERIFY_NODE to a node's name");
    S.skip("B42", what[1], "no node: set VERIFY_NODE to a node's name");
    return;
  }
  await S.check(
    "B41",
    what[0],
    async () => {
      await p.go(`${T.api}/`);
      const where = await p.waitFor(
        (n) => {
          const row = [...document.querySelectorAll("table.list tbody tr")].find((tr) => tr.querySelector("td:first-child a")?.textContent === n);
          return row ? row.children[2]?.textContent : null;
        },
        [T.box.name],
        { ms: 20_000, what: `the Where cell of ${T.box.name}` },
      );
      must(where === T.node, `the Where cell says ${where}`);
      await p.go(`${T.api}/s/${T.box.name}`);
      const facts = await p.waitFor(() => (document.querySelector("dl.facts")?.textContent.includes("Runs on") ? document.querySelector("dl.facts").textContent : null), [], {
        ms: 20_000,
        what: "the sandbox's facts",
      });
      must(facts.includes(`node ${T.node}`), `no "node ${T.node}" in ${facts}`);
      must(/Isolation(plain Docker|Sysbox|none)/.test(facts), `no isolation in ${facts}`);
    },
    { alone: true },
  );
  if (!T.admin) return S.skip("B42", what[1], "this config isn't an admin, so it has no Nodes page");
  await S.check(
    "B42",
    what[1],
    async () => {
      await p.go(`${T.api}/nodes`);
      const row = await p.waitFor(
        (n) => {
          const tr = [...document.querySelectorAll("table.list tbody tr")].find((r) => r.querySelector(".strong")?.textContent === n);
          return tr ? { state: tr.children[1]?.textContent, html: tr.querySelector(".strong").innerHTML } : null;
        },
        [T.node],
        { ms: 20_000, what: `a row for node ${T.node}` },
      );
      must(row.state === "online", `its state is ${row.state}`);
      must(row.html === T.node, "its name isn't plain text");
    },
    { alone: true },
  );
}

export const ORDER = { signin, terminal, reconnect, desktop, agent, work, nodes, code, next, phone, revoke, console: consoleCheck };
