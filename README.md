# flex — a private GPU pool

Pool the GPUs scattered across your home rig, office servers, and cloud boxes
into one private cluster. Join with one line, then run jobs on any free GPU from
anywhere — including a laptop with no GPU of its own.

The hard part of a GPU cluster is networking and setup, not scheduling. flex
puts that on a [Tailscale](https://tailscale.com) mesh, so NAT traversal and
secure connectivity come for free and what's left is small: inventory + a
scheduler + a CLI.

> Status: **M1** — the core loop (`join` → `gpus` → `run`) works end to end.
> Fair-share queueing, cancel/queue UX, and coordinator-restart replay land in M2.

## How it works

```
provider (GPU box)            coordinator (any box)         client (laptop, no GPU)
  flex agent  ──register──▶                         ◀──gpus──  flex gpus
              ──heartbeat─▶    SQLite assignment      ◀──jobs──  flex run
              ──poll jobs─▶       ledger (truth)
  run job  ◀── (pull)                                 client → agent: workdir tar (P2P)
  logs ──batch POST──▶  coordinator  ──SSE──▶  client streams logs, gets exit code
```

- **Control plane is pull-based**: agents poll the coordinator; it never dials back.
- **Data plane is P2P**: the client uploads the working directory straight to the
  assigned agent; the coordinator never carries the payload.
- **Trust boundary is the tailnet ACL.** `flex run` executes arbitrary commands
  on providers by design, so jobs run as an unprivileged user — keep providers
  off the public internet.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/PaulOh5/gpu-private-cloud-with-gstack/main/install.sh | sh
```

Or grab a binary from [Releases](https://github.com/PaulOh5/gpu-private-cloud-with-gstack/releases),
or build from source: `go build -o flex ./cmd/flex`.

## Quickstart

All machines must be on the same [tailnet](https://tailscale.com) first.

```sh
# 1. On any box (GPU not required), run the coordinator:
flex coordinator --addr :7070

# 2. On every machine, point at the coordinator (MagicDNS name recommended):
flex join http://my-coord.tailnet:7070

# 3. On each GPU box, contribute its GPUs:
flex agent

# 4. From anywhere — even a GPU-less laptop:
flex gpus
flex run --gpu any -- python train.py
```

`flex run` ships your current directory to the assigned GPU, streams the logs
back, and exits with the remote command's exit code. If your connection drops
during a long job, reattach with `flex logs <job-id>` — the job keeps running.

### Common commands

| Command | What it does |
|---|---|
| `flex gpus` | List every GPU in the pool and whether it's free |
| `flex run --gpu 4090 -- cmd` | Run on a GPU whose model matches "4090" |
| `flex run --gpu count:2 -- cmd` | Run on a node with 2 free GPUs |
| `flex run --env 'source .venv/bin/activate' -- python t.py` | Activate an env first |
| `flex run --detach -- cmd` | Submit and return immediately (reattach with `flex logs`) |
| `flex cancel <job-id>` | Stop a running job |

## Development

```sh
go test ./...   # unit + integration + localhost mock-GPU e2e (no hardware needed)
```

`FLEX_MOCK_GPUS="RTX 4090,A6000" flex agent` fakes inventory so you can run the
whole thing on a machine with no NVIDIA GPU.
