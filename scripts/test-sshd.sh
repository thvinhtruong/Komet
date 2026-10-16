#!/usr/bin/env bash
# Starts a throwaway sshd on 127.0.0.1:2222 that accepts a generated test key
# for the current user, plus a fake nvidia-smi, so remote targets can be
# tested on one machine. Linux, run as the user the "remote" commands run as.
set -euo pipefail
T=${T:-/tmp/komet-remote-test}
mkdir -p "$T/sshd" "$T/bin"
[ -f "$T/id_test" ] || ssh-keygen -q -t ed25519 -N '' -f "$T/id_test"
[ -f "$T/sshd/host_key" ] || ssh-keygen -q -t ed25519 -N '' -f "$T/sshd/host_key"
mkdir -p ~/.ssh && chmod 700 ~/.ssh
grep -qF "$(cat "$T/id_test.pub")" ~/.ssh/authorized_keys 2>/dev/null || cat "$T/id_test.pub" >>~/.ssh/authorized_keys
chmod 600 ~/.ssh/authorized_keys
cat >"$T/sshd/sshd_config" <<EOF
Port 2222
ListenAddress 127.0.0.1
HostKey $T/sshd/host_key
PermitRootLogin prohibit-password
PasswordAuthentication no
PubkeyAuthentication yes
AuthorizedKeysFile $HOME/.ssh/authorized_keys
AllowTcpForwarding yes
PidFile $T/sshd/sshd.pid
StrictModes no
UsePAM no
SetEnv PATH=$T/bin:/usr/local/bin:/usr/bin:/bin
EOF
cat >"$T/bin/nvidia-smi" <<'EOF'
#!/bin/sh
if [ "$#" -gt 0 ]; then echo "NVIDIA GeForce RTX 4090, 24564, 550.54.15"
else echo "| NVIDIA-SMI 550.54.15   Driver Version: 550.54.15   CUDA Version: 12.4 |"; fi
EOF
chmod +x "$T/bin/nvidia-smi"
sudo -n mkdir -p /run/sshd 2>/dev/null || mkdir -p /run/sshd
[ -f "$T/sshd/sshd.pid" ] && kill "$(cat "$T/sshd/sshd.pid")" 2>/dev/null || true
"$(command -v sshd || echo /usr/sbin/sshd)" -f "$T/sshd/sshd_config" -E "$T/sshd/log"
echo "test sshd on 127.0.0.1:2222, key $T/id_test"
