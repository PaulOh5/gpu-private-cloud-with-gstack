# Changelog

All notable changes to flex are documented here. Format follows
[Keep a Changelog](https://keepachangelog.com/); versions are `MAJOR.MINOR.PATCH.MICRO`.

## [0.1.0.0] - 2026-05-29

First milestone (M1): pool scattered GPUs and run jobs on any free one.

### Added
- `flex` single binary with subcommands: `coordinator`, `join`, `agent`, `gpus`, `run`, `logs`, `cancel`, `version`.
- **Coordinator** (control plane): HTTP/JSON API over a SQLite store (WAL). Tracks nodes, GPUs, and jobs; the assignment ledger is the source of truth for GPU availability.
- **Provider agent**: detects NVIDIA GPUs via `nvidia-smi`, registers with the coordinator, and pulls assigned jobs (the coordinator never dials back). Runs each job as a subprocess, pins it to its assigned GPU via `CUDA_VISIBLE_DEVICES`, and streams batched output back.
- **Client**: `flex gpus` lists the pool; `flex run` ships the working directory straight to the assigned agent (P2P), streams logs over SSE, and exits with the remote command's exit code. `flex logs <job>` reattaches to a running job; `flex cancel <job>` stops one.
- Concurrency-safe GPU assignment via SQLite `BEGIN IMMEDIATE` (race-tested): two clients contending for the last GPU yield exactly one winner.
- Jobs run as an unprivileged user; the whole process group is killed on cancel so child processes die too.
- Distribution: `install.sh` one-line installer, `goreleaser` config (cgo-free static binaries for linux/amd64, linux/arm64, darwin/arm64), and GitHub Actions for CI + release.
- `FLEX_MOCK_GPUS` lets the agent run without NVIDIA hardware (used by the localhost end-to-end test).

### Known limitations (targeted for M2)
- No reaper for jobs whose client dies mid-upload — a reserved GPU can stay stranded until the coordinator restarts.
- In-memory log hub has no TTL; job records accumulate until the coordinator restarts.
- Job status posts are not node-scoped (acceptable within the tailnet trust boundary, hardening deferred).
- Queued jobs are assigned only at submit time; background re-scheduling as GPUs free up lands in M2.
