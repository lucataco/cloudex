#!/bin/bash
# PID 1 for sandboxes. Starts sshd and dockerd, writes a ready marker tied to
# this boot, and runs the optional SIGTERM hook before exiting. The host allows
# 15 minutes between SIGTERM and SIGKILL.
set -u

log() { echo "[cloudex] $(date +%s.%N) $*" | tee -a /var/log/cloudex.log; }
log "entrypoint start sandbox=${CLOUDEX_SANDBOX:-?}"

# The platform's hostname is the 64-character instance ID, which is too long for
# a DNS label and breaks tools such as Python's http.server.
[ -n "${CLOUDEX_SANDBOX:-}" ] && hostname "${CLOUDEX_SANDBOX}"

# Trust the egress CA when HTTPS interception is on.
if [ -f /etc/cloudflare/certs/cloudflare-containers-ca.crt ]; then
  cp /etc/cloudflare/certs/cloudflare-containers-ca.crt /usr/local/share/ca-certificates/cloudflare-containers-ca.crt
  update-ca-certificates >/dev/null 2>&1
  log "trusted egress CA"
fi

# Persist the environment for exec() and SSH sessions, which don't inherit it.
for name in $(compgen -e | grep -E '^(CLOUDEX_|ANTHROPIC_|OPENAI_|DISABLE_TELEMETRY|CLAUDE_CODE_)'); do
  printf 'export %s=%q\n' "$name" "${!name}"
done > /etc/profile.d/cloudex-env.sh

# The API adds client keys to authorized_keys when they connect; a resumed
# disk keeps them. Host keys are made on first boot and kept the same way.
install -d -m 700 -o cloudex -g cloudex /home/cloudex/.ssh
ssh-keygen -A >/dev/null 2>&1
# /run is a tmpfs mounted at boot, so the directory baked into the image is hidden.
mkdir -p /run/sshd
/usr/sbin/sshd -E /var/log/sshd.log
log "sshd up"

if [ "${CLOUDEX_DOCKER:-1}" = 1 ]; then
  dockerd --iptables=false --ip6tables=false --ip-forward=false >/var/log/dockerd.log 2>&1 &
  log "dockerd launched"
fi

on_term() {
  log "SIGTERM"
  if [ -x /usr/local/bin/cloudex-on-term ]; then
    /usr/local/bin/cloudex-on-term >>/var/log/cloudex.log 2>&1
    log "on-term hook exited $?"
  fi
  exit 0
}
trap on_term TERM INT

/usr/local/bin/cloudex-keepalive >>/var/log/cloudex.log 2>&1 &

cat /proc/sys/kernel/random/boot_id > /run/cloudex-ready
log "ready"
while :; do
  sleep 3600 &
  wait $!
done
