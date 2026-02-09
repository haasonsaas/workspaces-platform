#!/usr/bin/env bash
set -euo pipefail

USERNAME="${WORKSPACES_USERNAME:-dev}"
AUTHORIZED_KEYS_FILE="${WORKSPACES_AUTHORIZED_KEYS_FILE:-/etc/workspaces/authorized_keys}"

if ! id -u "$USERNAME" >/dev/null 2>&1; then
  # UID/GID policy is intentionally simple for MVP; refine later.
  useradd -m -s /bin/bash "$USERNAME"
fi

HOME_DIR="$(getent passwd "$USERNAME" | cut -d: -f6)"
mkdir -p "$HOME_DIR"
chown -R "$USERNAME":"$USERNAME" "$HOME_DIR" || true

mkdir -p "$HOME_DIR/.ssh"
chmod 700 "$HOME_DIR/.ssh"
if [[ -f "$AUTHORIZED_KEYS_FILE" ]]; then
  cp "$AUTHORIZED_KEYS_FILE" "$HOME_DIR/.ssh/authorized_keys"
  chmod 600 "$HOME_DIR/.ssh/authorized_keys"
  chown "$USERNAME":"$USERNAME" "$HOME_DIR/.ssh/authorized_keys"
fi

# Host keys: generate if missing. Persist them on the home volume so reconnects
# don't constantly break known_hosts / VS Code Remote.
HOSTKEY_DIR="/home/.workspaces/ssh-host-keys"
mkdir -p "$HOSTKEY_DIR"

if [[ ! -f "$HOSTKEY_DIR/ssh_host_ed25519_key" ]]; then
  ssh-keygen -t ed25519 -N "" -f "$HOSTKEY_DIR/ssh_host_ed25519_key" >/dev/null
fi
if [[ ! -f "$HOSTKEY_DIR/ssh_host_rsa_key" ]]; then
  ssh-keygen -t rsa -b 3072 -N "" -f "$HOSTKEY_DIR/ssh_host_rsa_key" >/dev/null
fi

cat > /etc/ssh/sshd_config.d/workspaces-hostkeys.conf <<EOF
HostKey $HOSTKEY_DIR/ssh_host_ed25519_key
HostKey $HOSTKEY_DIR/ssh_host_rsa_key
AllowUsers $USERNAME
EOF

exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config

