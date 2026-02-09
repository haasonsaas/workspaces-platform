FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      git \
      openssh-server \
      sudo \
    && rm -rf /var/lib/apt/lists/*

# Minimal, predictable SSH config. We run sshd on 2222 so the pod can stay unprivileged
# even if you later choose to run the container as non-root.
COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /usr/local/bin/workspaces-desktop-entrypoint
RUN chmod +x /usr/local/bin/workspaces-desktop-entrypoint && \
    mkdir -p /run/sshd

EXPOSE 2222

ENTRYPOINT ["/usr/local/bin/workspaces-desktop-entrypoint"]

