#!/usr/bin/env bash
# End-to-end demo with the fake server standing in for llama-server and a
# cloud provider. Uses a throwaway KOMET_HOME so your real config is untouched.
set -euo pipefail
cd "$(dirname "$0")/.."

export KOMET_HOME="$(mktemp -d)"
export FAKE_KEY=demo
mkdir -p "$KOMET_HOME/models"
touch "$KOMET_HOME/models/fake-1.5b.gguf"

./bin/fakeserver --role cloud --port 18555 >"$KOMET_HOME/cloud.log" 2>&1 &
CLOUD=$!
trap './bin/komet stop >/dev/null 2>&1 || true; kill $CLOUD 2>/dev/null || true; rm -rf "$KOMET_HOME"' EXIT

cat >"$KOMET_HOME/config.json" <<EOF
{
  "local": {"enabled": true, "server_bin": "$PWD/bin/fakeserver",
    "models": [{"name": "fake-1.5b", "path": "$KOMET_HOME/models/fake-1.5b.gguf", "ram_mb": 1600}],
    "threads": 2, "ctx": 2048, "headroom_mb": 1024, "nice": 10, "keep_alive_sec": 30},
  "cloud": {"enabled": true, "base_url": "http://127.0.0.1:18555/v1", "model": "fake-cloud",
    "api_key_env": "FAKE_KEY", "price_in_per_mtok": 0.30, "price_out_per_mtok": 1.20, "json_mode": true},
  "policy": {"min_confidence": 0.8, "privacy": "redact", "hard_prompt_chars": 4000,
    "max_cloud_usd": 0.05, "require_known_binaries": true}
}
EOF
sleep 0.3
J=./bin/komet
step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

step "cold start: provisions the local model";     $J -v "find files larger than 100MB"
step "warm: model already loaded";                  $J "list files here"
step "small model invents a tool -> escalation";    $J -v "hallucinate: find TODO comments in go files"
step "syntax error -> escalation";                  $J -v "broken: list go files"
step "destructive command is flagged";              $J "clean the build dir"
step "secrets are masked before the cloud";         printf 'DATABASE_URL=postgres://app:S3cret@db/prod\nERROR: connection refused\n' | $J ask --cloud "why is this failing?"
step "deep question skips the local draft";         $J ask -v "design a sharded architecture for our job queue"
step "status";                                      $J status
step "stats";                                       $J stats
