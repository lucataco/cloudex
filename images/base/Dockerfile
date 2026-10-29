# cloudex-base: the default sandbox image (linux/amd64). Keep it small: the
# container disk is 20 GB on standard-4 and the image comes out of that.
FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive LANG=C.UTF-8

RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl wget git openssh-server sudo tmux rsync jq unzip zip less \
      vim-tiny build-essential pkg-config python3 python3-pip python3-venv procps \
      iproute2 iputils-ping dnsutils htop file xz-utils zstd squashfs-tools pv \
      libnss-myhostname \
  && rm -rf /var/lib/apt/lists/*

# Node 22 for the agent CLIs.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
  && apt-get install -y --no-install-recommends nodejs \
  && rm -rf /var/lib/apt/lists/*

# Docker Engine. The guest has no iptables, so containers use --network=host.
RUN install -m 0755 -d /etc/apt/keyrings \
  && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
  && echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable" \
       > /etc/apt/sources.list.d/docker.list \
  && apt-get update && apt-get install -y --no-install-recommends \
       docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
  && rm -rf /var/lib/apt/lists/*

# Agent CLIs. Users sign in once inside the sandbox; the login survives stop and resume.
RUN npm install -g @anthropic-ai/claude-code @openai/codex && npm cache clean --force

# The sandbox user is uid 1000 (the API execs as 1000:1000), so drop the
# image's stock ubuntu user, which holds that id. sshd ends sessions whose
# client hasn't answered for 3 minutes.
RUN userdel -r ubuntu 2>/dev/null; useradd -m -u 1000 -s /bin/bash -G sudo,docker cloudex \
  && echo 'cloudex ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/cloudex \
  && rm -f /home/cloudex/.bash_logout \
  && mkdir -p /run/sshd \
  && sed -i 's/^#\?PasswordAuthentication .*/PasswordAuthentication no/' /etc/ssh/sshd_config \
  && printf 'ClientAliveInterval 30\nClientAliveCountMax 6\n' > /etc/ssh/sshd_config.d/cloudex.conf

COPY entrypoint.sh /usr/local/bin/cloudex-entrypoint
COPY bin/ /usr/local/bin/
ENTRYPOINT ["/usr/local/bin/cloudex-entrypoint"]
