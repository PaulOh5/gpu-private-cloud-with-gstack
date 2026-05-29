# TODOS

Work organized by component, then priority (P0 highest). Completed items at the bottom.

## Coordinator / scheduler (M2)

- **Reaper for stranded jobs**
  **Priority:** P1
  **What:** Background sweep that fails (and frees the GPU of) jobs stuck in `assigned`/`running` past a heartbeat/upload timeout.
  **Why:** Today a client that dies mid-upload, or a provider that dies mid-job, leaves the reserved GPU held forever. Under flaky clients the pool degrades to zero usable GPUs. (Adversarial review #7, #11-tail.)
  **Context:** Pair with agent re-register job-state replay (design doc M2). Heartbeat timeout already marks GPUs unavailable conceptually; needs the job-side transition + GPU release.

- **Background re-scheduling of queued jobs**
  **Priority:** P1
  **What:** A loop that assigns `queued` jobs as GPUs free up. Today assignment only happens at submit time, so a job submitted when full never runs.
  **Why:** `flex run` currently errors "queued: no free GPU" instead of waiting. Required for real multi-user fairness (per-user `ceil(GPU/2)` cap + FIFO).

- **Log hub TTL / eviction**
  **Priority:** P2
  **What:** Evict terminal jobs' log buffers from the in-memory hub after a TTL.
  **Why:** `logHub.jobs` grows unbounded; a long-lived coordinator accumulates every job's backlog ring. (Adversarial review #1.)

- **Node-scoped status integrity**
  **Priority:** P2
  **What:** Authenticate that a status/log POST comes from the node that owns the job.
  **Why:** Any tailnet member can currently mark another node's job terminal and free its GPU while the real process runs. Within the accepted tailnet trust boundary, but worth hardening. (Adversarial review #13.)

## Client / CLI (M2)

- **SSE reconnect cursor**
  **Priority:** P3
  **What:** Use `Last-Event-ID` so `flex logs` reconnect doesn't replay the whole backlog.
  **Why:** Mid-job reconnect reprints up to 1024 already-seen chunks. Cosmetic. (Adversarial review #14.)

- **Longer job IDs / submit idempotency**
  **Priority:** P3
  **What:** Widen the 6-byte job ID and make a re-submitted ID re-attach instead of 500.
  **Why:** Network blip on submit currently isn't idempotent. (Adversarial review #6.)

## Agent (v2)

- **Container/namespace job isolation** — **Priority:** P2 — stronger than the unprivileged-user model; lets untrusted jobs run safely.
- **`[N/A]` VRAM handling** — **Priority:** P3 — a driver quirk reporting `[N/A]` for `memory.total` becomes 0 and silently makes the GPU unschedulable with `--vram`. (Adversarial review #15.)
- **AMD / non-NVIDIA GPU detection** — **Priority:** P3.

## Completed

- M1 core loop: join → gpus → run, agent runner + pull loop, SQLite coordinator, CLI + workdir P2P, binary e2e, distribution pipeline. **Completed:** v0.1.0.0 (2026-05-29)
