// Package types defines the data structures shared across the three flex
// components — agent (provider), coordinator, and CLI (client). It is
// dependency-free on purpose: every other package imports it, so it must not
// import any of them (avoids import cycles and keeps the merge surface small,
// per the eng-review parallelization plan — T1 lands before A/B/C lanes).
package types

import (
	"strconv"
	"time"
)

// LogChunk is a batch of job output as it flows agent → coordinator → client.
// Stream is "stdout" or "stderr".
type LogChunk struct {
	Stream string `json:"stream"`
	Data   string `json:"data"`
}

// Role distinguishes how a machine participates in the pool. The two roles are
// decoupled: a GPU-less laptop is a pure client, a GPU box runs an agent and is
// a provider, and a machine can be both.
type Role string

const (
	RoleProvider Role = "provider" // contributes GPUs (runs `flex agent`)
	RoleClient   Role = "client"   // submits jobs only (no GPU required)
)

// GPU is a single accelerator reported by a provider via `nvidia-smi`.
//
// Availability is NEVER derived from Utilization (noisy, lagging). The
// coordinator's assignment ledger is the source of truth for "is this GPU
// free" — Utilization/MemUsedMB are display/telemetry only.
type GPU struct {
	NodeID      string `json:"node_id"`      // owning provider node
	Index       int    `json:"index"`        // nvidia-smi index, 0-based, local to the node
	Name        string `json:"name"`         // model, e.g. "NVIDIA GeForce RTX 4090"
	MemTotalMB  int    `json:"mem_total_mb"` // total VRAM in MiB
	MemUsedMB   int    `json:"mem_used_mb"`  // telemetry only
	UtilPercent int    `json:"util_percent"` // telemetry only, 0-100
}

// GlobalID uniquely identifies a GPU across the pool (node + local index).
func (g GPU) GlobalID() string { return g.NodeID + ":" + strconv.Itoa(g.Index) }

// Node is a participant registered with the coordinator. Providers report a
// non-empty GPUs slice; clients report none. LastHeartbeat drives liveness:
// the coordinator marks a node's GPUs unavailable after a missed-heartbeat
// threshold (see coordinator package).
type Node struct {
	ID            string    `json:"id"`   // stable id (e.g. tailnet MagicDNS name)
	Addr          string    `json:"addr"` // tailnet address, reachable by clients for P2P tar upload
	Role          Role      `json:"role"`
	GPUs          []GPU     `json:"gpus"` // empty for pure clients
	LastHeartbeat time.Time `json:"last_heartbeat"`
}

// IsProvider reports whether the node contributes at least one GPU.
func (n Node) IsProvider() bool { return n.Role == RoleProvider && len(n.GPUs) > 0 }

// JobState is the lifecycle of a submitted job.
//
//	           ┌──────────────── cancel ───────────────┐
//	           ▼                                        │
//	queued ─assign─▶ assigned ─start─▶ running ─exit 0─▶ succeeded
//	   │                  │                │
//	   │                  │                ├─exit≠0──▶ failed
//	   │                  │                └─cancel──▶ killed
//	   └── (waits for a free GPU; FIFO + per-user cap in M2)
//
// On heartbeat timeout of the owning node, a running/assigned job is moved to
// failed (M2 adds re-register replay so a coordinator restart does not orphan
// jobs that are actually still running).
type JobState string

const (
	JobQueued    JobState = "queued"    // accepted, waiting for a free GPU
	JobAssigned  JobState = "assigned"  // a GPU is reserved, agent not yet started
	JobRunning   JobState = "running"   // subprocess executing on the provider
	JobSucceeded JobState = "succeeded" // exited 0
	JobFailed    JobState = "failed"    // exited non-zero, or node died mid-run
	JobKilled    JobState = "killed"    // cancelled via `flex cancel`
)

// IsTerminal reports whether the state is final (no further transitions).
func (s JobState) IsTerminal() bool {
	switch s {
	case JobSucceeded, JobFailed, JobKilled:
		return true
	default:
		return false
	}
}

// GPUSpec is the parsed form of `--gpu` on `flex run`.
//
//	any         → AnyModel, Count defaults to 1
//	4090        → Model="4090" (substring match against GPU.Name), Count 1
//	count:2     → Count=2, any model
type GPUSpec struct {
	AnyModel bool   `json:"any_model"`
	Model    string `json:"model"` // substring matched case-insensitively against GPU.Name
	Count    int    `json:"count"` // number of GPUs requested, >= 1
}

// Job is a unit of work submitted by a client and executed by an agent.
//
// The code is delivered client→agent directly (P2P over the tailnet); the
// coordinator only carries control state, never the workdir payload. EnvSetup
// runs on the provider before Command so the job matches the provider's
// runtime (venv/conda/CUDA) — v1 assumes the provider supplies the environment.
type Job struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	User       string    `json:"user"`      // --user or OS username; trust-based in v1
	Command    []string  `json:"command"`   // argv, e.g. ["python","train.py"]
	EnvSetup   string    `json:"env_setup"` // optional shell run before Command (e.g. "source .venv/bin/activate")
	Spec       GPUSpec   `json:"spec"`
	VRAMMinMB  int       `json:"vram_min_mb"` // 0 = no minimum
	State      JobState  `json:"state"`
	NodeID     string    `json:"node_id"`     // assigned provider, empty until assigned
	GPUIndexes []int     `json:"gpu_indexes"` // assigned GPU indexes on that node
	ExitCode   int       `json:"exit_code"`   // valid once terminal; propagated to `flex run`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}
