// The desktop: an X session in the sandbox that a browser shows through noVNC.
// The packages (TigerVNC, noVNC, openbox, tint2, xterm and Google Chrome, about
// 950 MB installed) go in the first time it's asked for, in about a minute, so
// the image stays small; a save keeps them, a restore from backup doesn't.
//
// Xtigervnc (called by name: the Xvnc alternative may point at another server)
// listens on localhost:5901 only. websockify serves noVNC and the VNC
// WebSocket on DESKTOP_PORT, which is reachable only through the API's TCP
// bridge (admin key) or a preview link minted for that port, so there is no VNC
// password. While a viewer is connected, a small loop reports activity so idle
// stop waits. Like the backup scripts, these run as root through exec, so any
// image can have a desktop without being rebuilt.

export const DESKTOP_PORT = 6080;

// noVNC options: scale the remote screen into the window, and favour bandwidth
// over photo quality (text is sent losslessly anyway). Measured in spikes/RESULTS.md (S5).
export const DESKTOP_VIEWER = "vnc.html?autoconnect=1&resize=scale&quality=3&compression=2&reconnect=1&path=websockify";

// $1 is the screen size (WxH), $2 the desktop's name. Starts whatever isn't
// running and resizes a running screen. Prints {"installed":bool,"started":bool}.
export const DESKTOP_SCRIPT = String.raw`
set -euo pipefail
size=$1 name=$2
log=/var/log/cloudex-desktop.log
exec 9>/run/cloudex-desktop.lock
flock 9
note() { echo "[desktop] $(date +%T.%N | cut -c1-12) $*" >>"$log"; }

up() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
wait_up() { for _ in $(seq 100); do up "$1" && return; sleep 0.1; done; echo "$2 didn't start; see $log" >&2; exit 1; }
as_user() { runuser -u cloudex -- env HOME=/home/cloudex USER=cloudex LOGNAME=cloudex LANG=C.UTF-8 DISPLAY=:1 "$@"; }
# Programs on the desktop start in the home directory, without the lock.
daemon() { (cd /home/cloudex && as_user setsid -f "$@" >>"$log" 2>&1 </dev/null 9>&-); }

installed=false
for c in Xtigervnc websockify openbox tint2 xterm xrandr xrdb google-chrome; do
  command -v "$c" >/dev/null || installed=true
done
[ -e /usr/share/novnc/vnc.html ] || installed=true
if $installed; then
  note "installing"
  export DEBIAN_FRONTEND=noninteractive
  curl -fsSL --retry 3 -o /tmp/google-chrome.deb https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
  note "downloaded chrome"
  apt-get update -qq >>"$log"
  note "apt updated"
  apt-get install -y -qq --no-install-recommends -o Dpkg::Options::=--force-unsafe-io \
    tigervnc-standalone-server tigervnc-tools novnc websockify openbox tint2 xterm x11-xserver-utils \
    fonts-dejavu-core fonts-liberation fonts-noto-color-emoji /tmp/google-chrome.deb >>"$log" 2>&1
  rm -f /tmp/google-chrome.deb
  note "installed"
fi

install -d -m 755 /etc/cloudex
cat >/etc/profile.d/cloudex-desktop.sh <<'EOF'
# Programs started from a shell show their windows on the desktop while it runs.
if [ -z "$DISPLAY" ] && [ -S /tmp/.X11-unix/X1 ]; then export DISPLAY=:1; fi
EOF
cat >/etc/cloudex/Xresources <<'EOF'
XTerm*faceName: DejaVu Sans Mono
XTerm*faceSize: 11
XTerm*background: #0b0a09
XTerm*foreground: #f3eee8
XTerm*cursorColor: #f8964a
XTerm*selectToClipboard: true
XTerm*saveLines: 10000
XTerm*scrollBar: false
EOF
cat >/etc/cloudex/tint2rc <<'EOF'
panel_items = LTSC
panel_size = 100% 30
panel_margin = 0 0
panel_padding = 4 2 4
panel_position = bottom center horizontal
panel_layer = top
panel_dock = 0
wm_menu = 1
panel_background_id = 1
font_shadow = 0

rounded = 0
border_width = 0
background_color = #15120f 100
border_color = #2b2622 100

rounded = 0
border_width = 0
background_color = #2b2622 100
border_color = #2b2622 100

launcher_padding = 2 2 6
launcher_background_id = 0
launcher_icon_size = 22
launcher_item_app = /usr/share/applications/google-chrome.desktop
launcher_item_app = /usr/share/applications/debian-xterm.desktop

taskbar_mode = single_desktop
taskbar_padding = 2 0 4
taskbar_background_id = 0
task_icon = 1
task_text = 1
task_centered = 0
task_maximum_size = 220 26
task_padding = 6 2 6
task_font = DejaVu Sans 9
task_font_color = #9a938b 100
task_active_font_color = #f3eee8 100
task_background_id = 0
task_active_background_id = 2

systray_padding = 0 0 4
systray_background_id = 0

time1_format = %H:%M
time1_font = DejaVu Sans Mono 10
clock_font_color = #9a938b 100
clock_padding = 6 0
clock_background_id = 0
EOF
cat >/usr/local/bin/cloudex-desktop-keepalive <<'EOF'
#!/bin/bash
# Counts a viewer connected to the desktop as activity, so idle stop waits.
# Only connections that sent something in the last 5 minutes count: a viewer
# asks for updates all the time, and a dropped one can look open for a long time.
while sleep 60; do
  if ss -Htin state established '( sport = :6080 )' | grep -oE 'lastrcv:[0-9]+' |
    awk -F: '$2 < 300000 { live = 1 } END { exit !live }'; then
    curl -fsS -m 10 -X POST http://cloudex.internal/activity >/dev/null 2>&1
  fi
done
EOF
chmod 755 /usr/local/bin/cloudex-desktop-keepalive

started=false
if ! up 5901; then
  note "starting Xtigervnc at $size"
  rm -f /tmp/.X1-lock /tmp/.X11-unix/X1
  daemon Xtigervnc :1 -geometry "$size" -depth 24 -SecurityTypes None -localhost yes -rfbport 5901 \
    -AlwaysShared -FrameRate 30 -desktop "$name"
  wait_up 5901 Xtigervnc
  started=true
else
  # Already running: switch the screen to the size asked for, adding a mode if needed.
  current=$(as_user xrandr | sed -n 's/.*current \([0-9]*\) x \([0-9]*\).*/\1x\2/p')
  if [ "$current" != "$size" ]; then
    note "resizing from $current to $size"
    out=$(as_user xrandr | awk '/ connected/ { print $1; exit }')
    w=$(echo "$size" | cut -dx -f1) h=$(echo "$size" | cut -dx -f2)
    as_user xrandr --newmode "$size" 60 "$w" $((w + 1)) $((w + 2)) $((w + 3)) "$h" $((h + 1)) $((h + 2)) $((h + 3)) 2>/dev/null || true
    as_user xrandr --addmode "$out" "$size" 2>/dev/null || true
    as_user xrandr --output "$out" --mode "$size" >>"$log" 2>&1 || echo "couldn't resize the running desktop to $size" >&2
  fi
fi
pgrep -u cloudex -x openbox >/dev/null || daemon openbox
pgrep -u cloudex -x tint2 >/dev/null || daemon tint2 -c /etc/cloudex/tint2rc
if $started; then
  as_user xsetroot -solid '#1d1915'
  as_user xrdb -merge /etc/cloudex/Xresources
  daemon google-chrome --no-first-run --no-default-browser-check --password-store=basic --start-maximized \
    --hide-crash-restore-bubble --user-data-dir=/home/cloudex/.config/cloudex-chrome --remote-debugging-port=9222
fi
up 6080 || daemon websockify --web /usr/share/novnc 6080 127.0.0.1:5901
wait_up 6080 websockify
pgrep -f '^/bin/bash /usr/local/bin/cloudex-desktop-keepalive' >/dev/null || setsid -f /usr/local/bin/cloudex-desktop-keepalive >/dev/null 2>&1 </dev/null 9>&-
note "ready"
printf '{"installed":%s,"started":%s}\n' "$installed" "$started"
`;

// Stops the desktop and everything shown on it.
export const DESKTOP_STOP_SCRIPT = String.raw`
pkill -f '^/bin/bash /usr/local/bin/cloudex-desktop-keepalive' || true
pkill -u cloudex -f '^/usr/bin/python3 /usr/bin/websockify' || true
pkill -u cloudex -x Xtigervnc || true
for _ in $(seq 50); do pgrep -u cloudex -x Xtigervnc >/dev/null || break; sleep 0.1; done
pkill -u cloudex -x tint2 || true
pkill -u cloudex -x openbox || true
pkill -u cloudex -f '^/opt/google/chrome/chrome' || true
rm -f /tmp/.X1-lock /tmp/.X11-unix/X1
echo '{"stopped":true}'
`;
