#!/bin/bash
# PID 1 for sandboxes. Starts sshd and dockerd, writes a ready marker tied to
# this boot, and runs the optional SIGTERM hook before exiting. The host allows
# 15 minutes between SIGTERM and SIGKILL.
set -u

log() { echo "[cloudex] $(date +%s.%N) $*" | tee -a /var/log/cloudex.log; }
log "entrypoint start box=${CLOUDEX_BOX:-?}"

# The platform's hostname is the 64-character instance ID, which is too long for
# a DNS label and breaks tools such as Python's http.server.
[ -n "${CLOUDEX_BOX:-}" ] && hostname "cx-${CLOUDEX_BOX}"

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

mkdir -p /home/cloudex/.ssh
echo "${CLOUDEX_SSH_PUBKEY:-}" > /home/cloudex/.ssh/authorized_keys
chown -R cloudex:cloudex /home/cloudex/.ssh
chmod 700 /home/cloudex/.ssh && chmod 600 /home/cloudex/.ssh/authorized_keys
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

cat /proc/sys/kernel/random/boot_id > /run/cloudex-ready
log "ready"
while :; do
  sleep 3600 &
  wait $!
done
