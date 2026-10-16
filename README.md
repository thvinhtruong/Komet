# Komet

**Local-first AI for your terminal.** Komet runs a small model on your own machine inside a resource envelope you control, checks every answer with cheap deterministic gates, and escalates to a cloud model only when the local draft is not good enough.

```
$ komet "find files larger than 100MB"
find . -type f -size +100M
  ↳ Lists files larger than 100 MB.
  local · qwen2.5-coder-1.5b-q4 · 0.2s · $0.0000 · gates ✓
```

Status: **prototype (v0.2)**. Single static Go binary, no dependencies beyond the standard library. Remote targets use your system `ssh` (and the `aws` CLI for EC2).

## How a request flows

```
request ─► guard ─► hardware check ─► difficulty check
             │            │                 │
   mask secrets   pick largest model   long/deep prompts
   for the cloud  that fits free RAM   skip the local draft
                  minus headroom
                         │
                         ▼
          local draft  (llama-server: grammar-constrained JSON,
                        prompt cache, bounded threads/ctx, nice 10)
                         │
                         ▼
          quality gates  (JSON parse · bash -n · shellcheck · binaries exist
                          on this machine · token confidence)
                 │                       │
              pass ✓                  fail ✗
                 │                       │
              answer        cloud leg gets the request + the failed draft
                            + the exact gate failures, returns a fix
```

Every leg is written to `~/.komet/ledger.jsonl` (tier, model, latency, tokens, cost, gate result), so `komet stats` can show your real local accept rate, the top failure reasons and the estimated saving versus sending everything to the cloud.

## Provisioning: fast up, fully down

The first local request spawns a small detached supervisor that starts `llama-server` with:

- `-t/-tb <threads>` and `-np 1`: bounded CPU and a single KV cache
- `-c <ctx>`: context window caps KV-cache memory
- process priority `nice 10`: your foreground apps win any CPU contention
- optional kernel-enforced caps (`cgroup_limits: true`, Linux): `systemd-run --user --scope -p MemoryMax=… -p CPUQuota=…`

Clients touch an activity file while they use the model. After `keep_alive_sec` with no activity, the supervisor stops the server and exits: **memory goes back to zero**. Repeat requests in a burst reuse the warm model and skip the load entirely. Model weights are mmapped, so even a cold reload is mostly served from the OS page cache.

Before loading, Komet reads available RAM (Linux `/proc/meminfo` + cgroup limits, macOS `vm_stat`) and load average. If the machine is busy or the model would not fit with your headroom, it skips local and goes to the cloud, or picks a smaller model.

## Quick start

```bash
go build -o komet ./cmd/komet      # or: make build
./komet init                          # writes ~/.komet/config.json
```

Local tier, pick one:

- **Managed (recommended):** install [llama.cpp](https://github.com/ggml-org/llama.cpp) so `llama-server` is on PATH, and put a GGUF model where `local.models[].path` points (default `~/.komet/models/`).
- **Bring your own server:** set `local.endpoint` to any OpenAI-compatible server you already run, e.g. Ollama (`http://127.0.0.1:11434/v1`, plus `endpoint_model`) or LM Studio. Komet then routes and gates but does not provision. This is also the path on Windows for now.

Cloud tier: any OpenAI-compatible endpoint (DeepSeek, OpenAI, OpenRouter, Ollama Cloud, …). Set `cloud.base_url`, `cloud.model`, and export the key named in `cloud.api_key_env`.

```bash
export DEEPSEEK_API_KEY=...
./komet doctor        # what is usable right now, and why not
```

## Usage

```bash
komet "compress every png in this folder"          # → one shell command
komet -r "show disk usage by directory"            # run it after confirmation
komet ask "what does set -euo pipefail do?"        # free-form question
cat error.log | komet ask "why is this failing?"   # piped context (secrets masked)
komet --local "..."      # never touch the network
komet --cloud "..."      # skip the local draft
komet -v "..."           # show every leg and why it was routed that way
komet status | stop | stats
```

stdout carries only the answer, everything else goes to stderr, so `cmd=$(komet "…")` works.

Safety: destructive patterns (`rm -r`, `dd of=/dev/…`, `curl | sh`, force-push, `mkfs`, …) are flagged, and `--run` requires typing `yes` for them and for any answer that did not pass the gates.

## Configuration (`~/.komet/config.json`)

| Key | Meaning |
|---|---|
| `local.models[]` | candidates, largest first: `name`, `path` (GGUF), `ram_mb` (resident estimate incl. KV cache) |
| `local.threads`, `local.ctx`, `local.nice` | the resource envelope |
| `local.headroom_mb` | RAM that must stay free for other apps |
| `local.max_load` | per-core load above which local is skipped (0 = ignore) |
| `local.keep_alive_sec` | unload after this much idle time |
| `local.cgroup_limits` | Linux: kernel-enforced memory/CPU caps via systemd-run (experimental) |
| `local.gpu_layers` | layers offloaded to GPU (`-ngl`) |
| `local.endpoint`, `local.endpoint_model` | use an existing server instead of provisioning |
| `cloud.base_url`, `cloud.model`, `cloud.api_key_env` | OpenAI-compatible escalation target |
| `cloud.price_in_per_mtok`, `cloud.price_out_per_mtok` | **set to your provider's current prices**; used for the ledger and budget |
| `policy.min_confidence` | geometric-mean token probability a local draft must reach (0 disables) |
| `policy.privacy` | `redact` (default: mask for public APIs), `private_only` (secrets reach only this machine and your private targets), `local_only` (secrets never leave this machine), `allow` |
| `targets[]` | remote machines, see [Remote targets](#remote-targets-your-vms-runpod-aws) |
| `budget.max_usd_per_day` | hourly-billed targets stop and refuse to start once today's spend reaches this |
| `policy.hard_prompt_chars` | longer inputs go straight to the cloud |
| `policy.max_cloud_usd` | per-request ceiling on estimated cloud spend |
| `policy.require_known_binaries` | treat "command not found here" as a gate failure |

## Remote targets: your VMs, RunPod, AWS

The same cascade can include machines you rent or own. Komet wakes them, provisions the model, remembers what it learned, connects through an SSH tunnel, and stops them when idle.

```
local (this machine) ──► your targets (private GPU) ──► public cloud API
  device trust            private trust: no masking       public trust: masked
  free                    billed per hour                 billed per token
```

```bash
komet up gpu           # wake + provision; prints where the time went
komet "..."            # automatic routing now includes gpu while it is up
komet -t gpu "..."     # this request only on gpu (wakes it if needed)
komet targets          # machine state, remembered hardware, last boot, spend
komet down gpu         # stop now (--keep: close the tunnel, leave it running)
```

**Routing rule.** A stopped target is never woken for a single automatic request. Waking takes from seconds to minutes and bills by the hour, so that is your call (`komet up`, `--target`). Once up, it sits between local and cloud in the cascade.

**What a session does**

1. *Wake*: provider API start (RunPod REST v2 / `aws ec2 start-instances`), wait for an address.
2. *SSH*: wait for login. Host keys are pinned per target; for cloud machines the pin is reset on each fresh boot (new container or recycled IP).
3. *Probe*: GPUs, VRAM, driver, CUDA, disk, whether the engine is installed. Saved as the target's **profile**.
4. *Choose the model*: the largest configured model that fits the measured VRAM.
5. *Install* (first time only): builds llama.cpp with CUDA, or `pip install vllm`, or your `install_cmd`, into the persistent work dir.
6. *Start*: downloads the model if missing (first time only), starts the server on the target's loopback with an API key, starts the watchdog.
7. *Tunnel*: `ssh -N -L`; the inference port is never exposed to the internet.

Each start records per-phase timings in the profile, so `komet up` shows where the time went and what the next start will cost. The second start skips install and download because both live on the persistent volume.

**Stopping is layered**, because forgetting a GPU is the expensive failure:

- local: no request for `autostop_idle_min` → stop server and machine (`on_idle: keep` only closes the tunnel);
- remote watchdog: no SSH session for the idle time plus a grace period, or `max_uptime_hours` reached → stops the server and the machine itself (AWS: `shutdown`; RunPod: API call with a dedicated key). This works even if your laptop is closed or offline;
- `budget.max_usd_per_day`: running sessions are stopped and new ones refused.

Every session is appended to `~/.komet/sessions.jsonl` with its duration and cost. `komet stats` and `komet targets` show today's spend.

### RunPod

```json
{"name": "gpu", "kind": "runpod", "pod_id": "abc123xyz",
 "key_path": "~/.ssh/id_ed25519", "self_stop_key_env": "RUNPOD_STOP_KEY",
 "models": [
   {"name": "qwen2.5-coder-32b-q4", "path": "/workspace/komet/models/qwen32b-q4.gguf",
    "url": "https://huggingface.co/<repo>/resolve/main/<file>.gguf", "vram_mb": 22000},
   {"name": "qwen2.5-coder-14b-q4", "path": "/workspace/komet/models/qwen14b-q4.gguf",
    "url": "https://huggingface.co/<repo>/resolve/main/<file>.gguf", "vram_mb": 11000}],
 "autostop_idle_min": 15, "max_uptime_hours": 8}
```

- `export RUNPOD_API_KEY=...` (Komet uses REST API v2; v1 retires on 2026-11-15).
- The pod needs a **public IP with TCP port 22 exposed** and your SSH public key. The proxied `ssh.runpod.io` connection cannot forward ports, so it cannot be used.
- Keep models and the engine under `/workspace` (the default work dir). That volume survives a stop; the container disk does not.
- `self_stop_key_env`: a second, restricted API key that is copied to the pod so the watchdog can stop it when your computer is gone. Without it, only your computer stops the pod.
- A stopped pod may not get its GPU back on start if the host is full; `komet up` then fails with RunPod's message.

### AWS (EC2)

```json
{"name": "g6", "kind": "aws", "instance_id": "i-0abc...", "region": "eu-central-1",
 "user": "ubuntu", "key_path": "~/.ssh/aws.pem", "cost_per_hour": 0.98,
 "work_dir": "~/komet", "models": [ ... ]}
```

- Uses the `aws` CLI, so profiles, SSO and roles work as they do for you (`profile` selects one).
- An existing **EBS-backed** GPU instance with a CUDA driver; the security group allows SSH from your IP. Stopping keeps the EBS volume (engine + weights) and stops compute billing; storage is still billed.
- Leave *shutdown behavior* at **stop** (the default) so the watchdog's `shutdown` stops the instance instead of terminating it.
- The public IP changes on every start unless you attach an Elastic IP; Komet reads the current one from the API (or set `host`).
- EC2 does not report prices through this API: set `cost_per_hour` so spend and budget are tracked.

### Any SSH machine

`{"name": "homelab", "kind": "ssh", "host": "10.0.0.5", "user": "me", "models": [...]}`. The machine is assumed to be always on: Komet starts and stops only the model server.

### Target options

| Key | Meaning |
|---|---|
| `kind` | `ssh`, `runpod`, `aws` |
| `trust` | `private` (default: requests are not masked) or `public` |
| `engine` | `llama` (default) or `vllm` (better throughput for many parallel agents; needs a GPU) |
| `models[]` | `name`, `path` on the target (vLLM: a Hugging Face repo id), `url` to download a GGUF, `vram_mb` |
| `work_dir` | persistent directory on the target (default `/workspace/komet` on RunPod, `~/komet` elsewhere) |
| `server_bin`, `install_cmd` | use your own engine build or install step |
| `ctx`, `parallel`, `gpu_layers` | server settings (defaults 8192, 4, all layers) |
| `autostop_idle_min`, `max_uptime_hours`, `on_idle` | lifecycle (defaults 15, 8, `stop`) |
| `cost_per_hour` | overrides the provider price (required for AWS) |
| `use_when_running` | `false` keeps the target out of automatic routing even when up |

## Develop and test without a model

`tools/fakeserver` imitates llama-server (and a cloud provider with `--role cloud`), including a load delay, `/health`, logprobs, and deliberately bad answers (prompts containing `hallucinate` or `broken`), so the full pipeline can be exercised offline:

```bash
make test         # unit tests: guard, gates, router, providers (RunPod v2, aws CLI), model choice
make demo         # end-to-end local tier with the fake server as llama-server
make remote-demo  # end-to-end remote targets (Linux): local sshd as the "GPU machine",
                  # fake RunPod API, fake aws CLI; covers wake, download, idle stop, watchdog
```

## Layout

```
cmd/komet            CLI
internal/router      N-tier cascade orchestration
internal/gate        quality gates + destructive-command detection
internal/guard       secret detection and masking
internal/provision   llama-server supervisor, keep-alive, resource envelope
internal/remote      targets: providers (ssh, RunPod v2, AWS), session supervisor,
                     SSH tunnel, profiles, spend; bootstrap.sh runs on the target
internal/proc        process detach / signals / priority / locks
internal/hw          RAM / load probes (Linux, macOS)
internal/llm         OpenAI-compatible client (structured output, logprobs, prompt cache)
internal/ledger      per-leg accounting and stats
tools/fakeserver     offline stand-in for llama-server and a cloud API
tools/fakerunpod     offline stand-in for the RunPod REST API v2
tools/fakeaws        offline stand-in for the aws CLI
```

## Roadmap

1. **Kometfile**: declarative workflow protocol (steps, gates, escalation conditions, per-step resource budgets) replacing the fixed cascade.
2. **Persisted prompt KV cache** (`--slot-save-path`) so even a cold start skips prefill of the fixed system prompt.
3. **More runtimes** behind the same interface: MLX on Apple Silicon, OpenVINO/NPU, Foundry Local.
4. **Eval harness**: replay the ledger against new thresholds and models to tune routing on your own workload.
5. **Critique pattern** and LoRA hot-swap roles (router, coder, critic) on one base model.
6. Remote: create machines from scratch (or via SkyPilot), GPU memory snapshots for second-scale wake, predictive pre-warm from the ledger.
7. Streaming output for `ask`, Windows-native provisioning, signed release binaries.
