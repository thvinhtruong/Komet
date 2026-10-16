#!/usr/bin/env bash
# End-to-end test of remote targets without any cloud account:
#   - a local sshd on 127.0.0.1:2222 plays the GPU machine
#   - tools/fakerunpod plays the RunPod REST API v2
#   - tools/fakeaws/aws plays the aws CLI
#   - tools/fakeserver plays llama-server on the "remote" side
# Requires: sshd running per scripts/test-sshd.sh, binaries in bin/.
set -uo pipefail
cd "$(dirname "$0")/.."
T=${T:-/tmp/komet-remote-test}
export KOMET_HOME=$T/home RUNPOD_API_KEY=rp-test-key RP_STOP=rp-test-key FAKE_KEY=x
export FAKE_AWS_STATE=$T/aws.state FAKE_AWS_BOOT_S=2
rm -rf "$KOMET_HOME" "$T"/ws-* "$T"/aws.state* "$T"/rp-actions.log "$T/www"
mkdir -p "$KOMET_HOME" "$T/www"

./bin/fakerunpod --port 18600 --boot-ms 2500 --actions-log "$T/rp-actions.log" >"$T/rp.log" 2>&1 &
P1=$!
./bin/fakeserver --role cloud --port 18555 >"$T/cloud.log" 2>&1 &
P2=$!
head -c 3000000 /dev/urandom >"$T/www/qwen-14b.gguf"
(cd "$T/www" && exec python3 -m http.server 18700 >/dev/null 2>&1) &
P3=$!
cleanup() { ./bin/komet down rp >/dev/null 2>&1; ./bin/komet down ec2 >/dev/null 2>&1; kill $P1 $P2 $P3 2>/dev/null; }
trap cleanup EXIT

cat >"$KOMET_HOME/config.json" <<EOF
{
  "local": {"enabled": false, "models": []},
  "targets": [
    {"name": "rp", "kind": "runpod", "pod_id": "pod-test-1", "api_base": "http://127.0.0.1:18600/v2",
     "self_stop_key_env": "RP_STOP", "key_path": "$T/id_test", "work_dir": "$T/ws-rp",
     "server_bin": "$PWD/bin/fakeserver",
     "models": [
       {"name": "qwen-72b-q4", "path": "$T/ws-rp/models/72b.gguf", "vram_mb": 48000},
       {"name": "qwen-14b-q4", "path": "$T/ws-rp/models/14b.gguf", "url": "http://127.0.0.1:18700/qwen-14b.gguf", "vram_mb": 12000}
     ],
     "autostop_idle_min": ${IDLE_MIN:-0.25}},
    {"name": "ec2", "kind": "aws", "instance_id": "i-0123456789abcdef0", "region": "eu-central-1",
     "aws_bin": "$PWD/tools/fakeaws/aws", "user": "root", "port": 2222, "key_path": "$T/id_test",
     "work_dir": "$T/ws-ec2", "server_bin": "$PWD/bin/fakeserver", "cost_per_hour": 1.006,
     "models": [{"name": "llama-8b-q8", "path": "$T/ws-rp/models/14b.gguf", "vram_mb": 10000}]}
  ],
  "cloud": {"enabled": true, "base_url": "http://127.0.0.1:18555/v1", "model": "fake-cloud", "api_key_env": "FAKE_KEY",
            "price_in_per_mtok": 0.3, "price_out_per_mtok": 1.2, "json_mode": true},
  "policy": {"min_confidence": 0.8, "privacy": "redact", "hard_prompt_chars": 4000, "max_cloud_usd": 0.05, "require_known_binaries": true},
  "budget": {"max_usd_per_day": 5}
}
EOF
sleep 0.5
K=./bin/komet
step() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

step "targets before anything ran";            $K targets
step "komet up rp  (first time: wake, probe, download model)"; $K up rp
step "auto routing uses the running pod";      $K -v "list files here"
step "a log with a secret goes unmasked to your private pod ..."
printf 'DATABASE_URL=postgres://app:S3cretPw@db/prod\nERROR: connection refused\n' | $K ask -v "why is this failing?"
step "... but is masked when it goes to the public cloud API"
printf 'DATABASE_URL=postgres://app:S3cretPw@db/prod\nERROR: connection refused\n' | $K ask --cloud "why is this failing?"
step "komet down rp";                          $K down rp
step "RunPod actions received";                cat "$T/rp-actions.log"
step "komet up rp  (second time: remembered, model already on the volume)"; $K up rp
step "idle autostop (waiting $(( ${IDLE_MIN_WAIT:-30} ))s)"; sleep "${IDLE_MIN_WAIT:-30}"; $K targets | sed -n '1,6p'
step "RunPod actions received";                cat "$T/rp-actions.log"
step "--target ec2 wakes the AWS instance for one request"; $K -v --target ec2 "find files larger than 100MB"
step "komet down ec2";                         $K down ec2
step "aws calls";                              cut -d' ' -f2 "$T/aws.state.log" | uniq -c
step "spend";                                  $K stats | tail -2
