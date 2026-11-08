// The desktop tab: starts the sandbox's desktop (POST /desktop; the first
// time installs it, a minute or two), then shows it with the dashboard's own
// noVNC over GET /desktop/vnc. The sandbox's noVNC page is never loaded here,
// since a page from the sandbox on this host could use the dashboard's cookie.
import type { Desktop as DesktopInfo, Sandbox } from "@cloudex/schema";
import { useEffect, useRef, useState } from "preact/hooks";
import { del, post, sb, wsUrl } from "../api";
import type { RFB, RFBClass } from "../novnc";
import { Act, Problem, errText } from "../ui";

let rfbClass: Promise<RFBClass> | undefined;
function loadRFB() {
  const src = document.querySelector<HTMLMetaElement>('meta[name="cx-vnc"]')!.content;
  rfbClass ??= import(/* @vite-ignore */ src).then((m: { default: RFBClass }) => m.default);
  return rfbClass;
}

const SIZES = ["1280x720", "1440x900", "1600x900", "1920x1080", "2560x1440"];

type Phase = "starting" | "connecting" | "connected" | "reconnecting" | "stopped";

export function Desktop(props: { box: Sandbox }) {
  const box = props.box;
  const screen = useRef<HTMLDivElement>(null);
  const rfb = useRef<RFB | null>(null);
  const [phase, setPhase] = useState<Phase>("starting");
  const [info, setInfo] = useState<DesktopInfo | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [fitWindow, setFitWindow] = useState(false);
  const [clip, setClip] = useState("");
  const st = useRef({ gone: false, failures: 0, timer: 0 as unknown as ReturnType<typeof setTimeout>, stopping: false });

  const start = async (size?: string) => {
    const d = await post<DesktopInfo>(`${sb(box.id)}/desktop`, size ? { size } : {});
    setInfo(d);
    return d;
  };

  const connect = async () => {
    const s = st.current;
    if (s.gone) return;
    clearTimeout(s.timer);
    const RFBc = await loadRFB();
    if (s.gone) return;
    rfb.current?.disconnect();
    screen.current!.replaceChildren();
    const r = new RFBc(screen.current!, wsUrl(`${sb(box.id)}/desktop/vnc`));
    r.scaleViewport = !fitWindow;
    r.resizeSession = fitWindow;
    r.qualityLevel = 4;
    r.compressionLevel = 2;
    r.focusOnClick = true;
    r.background = "#0b0a09";
    rfb.current = r;
    let connected = false;
    r.addEventListener("connect", () => {
      connected = true;
      s.failures = 0;
      setError(null);
      setPhase("connected");
      r.focus();
    });
    r.addEventListener("clipboard", (e) => setClip((e as CustomEvent<{ text: string }>).detail.text));
    r.addEventListener("disconnect", async () => {
      if (rfb.current === r) rfb.current = null;
      if (s.gone || s.stopping) return;
      if (!connected) s.failures++;
      setPhase("reconnecting");
      // Twice in a row: the desktop may have gone with a stop. Start it again.
      if (s.failures >= 2) {
        try {
          await start();
        } catch (e) {
          setError(errText(e));
        }
      }
      s.timer = setTimeout(() => void connect(), Math.min(10_000, 500 * 2 ** Math.min(s.failures, 5)));
    });
  };

  useEffect(() => {
    const s = st.current;
    s.gone = false;
    (async () => {
      try {
        await start();
        setPhase("connecting");
        await connect();
      } catch (e) {
        setError(errText(e));
        setPhase("stopped");
      }
    })();
    // Each frame request counts as activity already; this covers a still screen.
    return () => {
      s.gone = true;
      clearTimeout(s.timer);
      rfb.current?.disconnect();
      rfb.current = null;
    };
  }, [box.id]);

  useEffect(() => {
    if (!rfb.current) return;
    rfb.current.scaleViewport = !fitWindow;
    rfb.current.resizeSession = fitWindow;
  }, [fitWindow]);

  const stop = async () => {
    st.current.stopping = true;
    rfb.current?.disconnect();
    await del(`${sb(box.id)}/desktop`);
    setPhase("stopped");
  };
  const again = async () => {
    st.current.stopping = false;
    st.current.failures = 0;
    setPhase("starting");
    await start();
    setPhase("connecting");
    await connect();
  };

  return (
    <section class="desktop">
      <div class="toolbar">
        <span class="muted small">
          {phase === "starting"
            ? "Starting the desktop… the first time installs it, which takes a minute or two."
            : phase === "connecting"
              ? "Connecting…"
              : phase === "reconnecting"
                ? "Reconnecting…"
                : phase === "stopped"
                  ? "The desktop is stopped."
                  : ""}
        </span>
        {phase === "connected" ? (
          <>
            <label class="check small">
              <input type="checkbox" checked={fitWindow} onChange={(e) => setFitWindow(e.currentTarget.checked)} />
              Match this window's size
            </label>
            {fitWindow ? null : (
              <select
                aria-label="Screen size"
                value={info?.size ?? ""}
                onChange={(e) => start(e.currentTarget.value).catch((x) => setError(errText(x)))}
              >
                {[...new Set([...(info ? [info.size] : []), ...SIZES])].map((x) => (
                  <option value={x} key={x}>
                    {x}
                  </option>
                ))}
              </select>
            )}
            <button type="button" class="btn btn-quiet" onClick={() => screen.current?.requestFullscreen?.()}>
              Full screen
            </button>
            <Act run={stop} onError={setError} kind="quiet" busy="Stopping…">
              Stop the desktop
            </Act>
          </>
        ) : phase === "stopped" ? (
          <Act run={again} onError={setError} kind="primary" busy="Starting…">
            Start the desktop
          </Act>
        ) : null}
      </div>
      <Problem error={error} />
      <div class="vnc" ref={screen} hidden={phase === "stopped"} />
      <details class="clip">
        <summary>Clipboard</summary>
        <p class="muted small">What's copied on the desktop shows here. To paste on the desktop, put text here and send it, then paste there.</p>
        <textarea rows={3} value={clip} onInput={(e) => setClip(e.currentTarget.value)} spellcheck={false} />
        <button type="button" class="btn" onClick={() => rfb.current?.clipboardPasteFrom(clip)} disabled={phase !== "connected"}>
          Send to the desktop
        </button>
      </details>
    </section>
  );
}
