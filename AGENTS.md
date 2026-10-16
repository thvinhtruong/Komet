# Komet Agent Guide

## Project shape

Komet is a standard-library-only Go 1.24.7 CLI for local-first LLM routing. Start with the [README](README.md) for user-facing behavior, configuration, remote targets, and demos.

- `cmd/komet`: CLI parsing, command dispatch, lifecycle commands, and output handling.
- `internal/router`: ordered local, remote, and cloud tier orchestration, privacy checks, difficulty routing, and escalation.
- `internal/config`: configuration and `KOMET_HOME`-based state paths.
- `internal/gate` and `internal/guard`: answer safety/quality checks and secret detection/redaction.
- `internal/provision`: local `llama-server` lifecycle and resource limits.
- `internal/remote`: SSH, RunPod, and AWS providers, sessions, tunnels, and bootstrap.
- `internal/llm`: shared OpenAI-compatible HTTP client.
- `internal/hw` and `internal/proc`: platform-specific hardware and process behavior.
- `internal/ledger`: append-only JSONL accounting; `internal/ui`: terminal presentation.
- `tools/` and `scripts/`: offline fakes and demo/test orchestration.

Keep orchestration behind the existing interfaces so it remains testable with stubs and `httptest` servers. Preserve the platform build tags in `internal/hw` and `internal/proc`; do not assume Unix behavior in shared code.

## Commands

Use the Makefile targets when available:

```sh
make build        # build bin/komet
make test         # go vet ./... && go test ./...
make demo         # offline local end-to-end demo
make remote-demo  # Linux-oriented remote integration demo
make release      # cross-compile supported release binaries
```

Run `gofmt` on changed Go files. For focused work, use `go test ./internal/<package>` or a specific test before `make test`.

## Development rules

- Keep dependencies in the standard library unless a change explicitly requires otherwise.
- Add table-driven tests for behavior changes; use `t.TempDir`, `t.Setenv`, and `httptest` consistently with existing tests.
- Keep runtime directories at `0700` and state files at `0600`.
- Preserve stdout as answer-only output. Send diagnostics, progress, and spinner output to stderr.
- Treat ledger and session records as JSONL and preserve append-only behavior.
- Avoid leaking secrets when requests cross public tiers; route privacy decisions through the existing guard/router flow.
- Keep destructive-command checks and `--run` confirmation behavior intact unless the task explicitly changes the safety contract.
- Do not assume external models or cloud credentials are available. Prefer the fakes in `tools/` for offline tests and demos.

## Environment notes

Runtime state is under `~/.komet`, or `$KOMET_HOME` when set. Managed local inference needs `llama-server` and GGUF models; an OpenAI-compatible `local.endpoint` is the supported alternative, including on Windows. Remote operation may require system `ssh`, the AWS CLI, or RunPod credentials. Linux-only resource controls and remote demos should remain guarded by their existing platform checks.

Before changing routing, provisioning, or remote lifecycle behavior, read the nearest package tests and update focused coverage for failure, privacy, cleanup, and platform-sensitive paths.