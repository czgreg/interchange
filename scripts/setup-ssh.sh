#!/usr/bin/env bash
# scripts/setup-ssh.sh — one-time PoC node bootstrap.
#
# Pushes ~/.ssh/id_ed25519.pub (or id_rsa.pub) to the node's authorized_keys
# and installs a NOPASSWD sudoers dropin for the deploy commands. After this
# succeeds, all `make deploy-*` targets work without password prompts.
#
# Usage:
#   ./scripts/setup-ssh.sh                        # default: dianwei@192.168.70.92
#   ./scripts/setup-ssh.sh user@host
#
# Env:
#   SSH_PASSWORD  required for the initial password-auth ssh (default: ppio2026 for PoC)
#
# Idempotent — safe to re-run.

set -euo pipefail

NODE="${1:-dianwei@192.168.70.92}"
SUDO_PASS="${SSH_PASSWORD:-ppio2026}"

PUB=""
for k in ~/.ssh/id_ed25519.pub ~/.ssh/id_rsa.pub; do
  [ -f "$k" ] && PUB="$(cat "$k")" && break
done
[ -n "$PUB" ] || { echo "no ssh public key found in ~/.ssh/" >&2; exit 1; }

command -v expect >/dev/null || { echo "need 'expect' (brew install expect)" >&2; exit 1; }

echo "[setup-ssh] pushing pubkey to $NODE (will use password auth this time)"

expect <<EOF
set timeout 30
spawn ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \\
  $NODE "mkdir -p ~/.ssh && chmod 700 ~/.ssh && \\
    grep -qxF '$PUB' ~/.ssh/authorized_keys 2>/dev/null || echo '$PUB' >> ~/.ssh/authorized_keys && \\
    chmod 600 ~/.ssh/authorized_keys && echo KEY_OK"
expect {
  -re "(?i)password:" { send "$SUDO_PASS\r"; exp_continue }
  eof
}
catch wait result
exit [lindex \$result 3]
EOF

echo "[setup-ssh] verifying key auth"
ssh -o BatchMode=yes -o StrictHostKeyChecking=no "$NODE" 'echo SSH_KEY_OK' >/dev/null

echo "[setup-ssh] installing NOPASSWD sudoers dropin"
NODE_USER="${NODE%@*}"
ssh -o BatchMode=yes "$NODE" "echo '$SUDO_PASS' | sudo -S sh -c '
echo \"$NODE_USER ALL=(ALL) NOPASSWD: /usr/bin/systemctl, /usr/bin/install, /usr/bin/tar, /bin/bash, /usr/bin/journalctl, /usr/sbin/nft\" > /etc/sudoers.d/leap-deploy
chmod 0440 /etc/sudoers.d/leap-deploy
visudo -c -f /etc/sudoers.d/leap-deploy
'"

echo "[setup-ssh] verifying NOPASSWD sudo"
ssh -o BatchMode=yes "$NODE" 'sudo -n systemctl is-active leap-gateway >/dev/null && echo NOPASS_OK'

echo "[setup-ssh] DONE — try 'make deploy-fast'"
