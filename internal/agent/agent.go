package agent

import (
	"context"
	"sync"
	"time"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// Control is the coordinator-facing API the agent depends on. It is an
// interface so the poll loop is testable against a fake (no HTTP), and so the
// real HTTP client lives behind the same contract. Every call is pull-style:
// the agent reaches out; the coordinator never pushes to the agent.
type Control interface {
	Register(ctx context.Context, node types.Node) error
	Heartbeat(ctx context.Context, nodeID string) error
	// PollAssigned returns jobs the coordinator has assigned to this node that
	// are waiting to start (state == assigned).
	PollAssigned(ctx context.Context, nodeID string) ([]types.Job, error)
	// PollCancels returns IDs of this node's jobs the coordinator wants killed.
	PollCancels(ctx context.Context, nodeID string) ([]string, error)
	ReportStatus(ctx context.Context, jobID string, state types.JobState, exitCode int) error
	AppendLogs(ctx context.Context, jobID string, chunks []LogChunk) error
}

// Agent runs on a provider, contributing its GPUs and executing assigned jobs.
//
//	Run: Register(node) ─▶ loop every Interval:
//	       Heartbeat ─▶ PollAssigned ─▶ start new jobs (each in its own goroutine)
//	                 ─▶ PollCancels  ─▶ cancel matching running jobs
//	   per job goroutine: ReportStatus(running) ─▶ RunJob ─▶ ReportStatus(terminal)
type Agent struct {
	NodeID    string
	Addr      string // tailnet address clients use for P2P workdir upload
	Control   Control
	Interval  time.Duration // poll/heartbeat cadence
	RunAsUser string        // unprivileged user to run jobs as (provider safety)

	// Stager resolves the staged workdir (unpacked client tar) for a job. When
	// set, a job only starts once its workdir has been received. When nil, jobs
	// run in the agent's cwd (used by tests with self-contained commands).
	Stager Stager

	mu      sync.Mutex
	running map[string]context.CancelFunc // jobID -> cancel
}

// Run registers the node's GPUs, then loops until ctx is cancelled, driving the
// pull cycle. gpus is the inventory detected for this provider.
func (a *Agent) Run(ctx context.Context, gpus []types.GPU) error {
	a.running = map[string]context.CancelFunc{}
	if a.Interval <= 0 {
		a.Interval = 2 * time.Second
	}

	node := types.Node{
		ID: a.NodeID, Addr: a.Addr, Role: roleFor(gpus),
		GPUs: gpus, LastHeartbeat: time.Now(),
	}
	if err := a.Control.Register(ctx, node); err != nil {
		return err
	}

	t := time.NewTicker(a.Interval)
	defer t.Stop()
	for {
		a.tick(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// tick runs one pull cycle. Errors are non-fatal (logged by the HTTP client in
// prod); a transient coordinator blip should not kill the agent.
func (a *Agent) tick(ctx context.Context) {
	_ = a.Control.Heartbeat(ctx, a.NodeID)

	if assigned, err := a.Control.PollAssigned(ctx, a.NodeID); err == nil {
		for _, job := range assigned {
			a.startJob(ctx, job)
		}
	}
	if cancels, err := a.Control.PollCancels(ctx, a.NodeID); err == nil {
		for _, id := range cancels {
			a.cancelJob(id)
		}
	}
}

// startJob launches a job if it is not already running and its workdir is
// staged. If the Stager doesn't have the workdir yet (client tar still in
// flight), it returns without starting — the next poll retries.
func (a *Agent) startJob(parent context.Context, job types.Job) {
	workdir := ""
	if a.Stager != nil {
		d, ready := a.Stager.Dir(job.ID)
		if !ready {
			return // wait for the client→agent tar upload
		}
		workdir = d
	}

	a.mu.Lock()
	if _, ok := a.running[job.ID]; ok {
		a.mu.Unlock()
		return
	}
	jobCtx, cancel := context.WithCancel(parent)
	a.running[job.ID] = cancel
	a.mu.Unlock()

	go a.execute(jobCtx, job, workdir)
}

func (a *Agent) execute(ctx context.Context, job types.Job, workdir string) {
	defer func() {
		a.mu.Lock()
		delete(a.running, job.ID)
		a.mu.Unlock()
	}()

	a.Control.ReportStatus(ctx, job.ID, types.JobRunning, 0)

	res, err := RunJob(ctx, RunSpec{
		Command:    job.Command,
		EnvSetup:   job.EnvSetup,
		WorkDir:    workdir,
		RunAsUser:  a.RunAsUser,
		GPUIndexes: job.GPUIndexes,
	}, func(ch LogChunk) {
		// Best-effort; a dropped log chunk must not crash the job.
		a.Control.AppendLogs(context.WithoutCancel(ctx), job.ID, []LogChunk{ch})
	})

	state, code := terminalState(res, err)
	a.Control.ReportStatus(context.WithoutCancel(ctx), job.ID, state, code)
}

// cancelJob cancels a running job's context (kills its process group).
func (a *Agent) cancelJob(jobID string) {
	a.mu.Lock()
	cancel := a.running[jobID]
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// terminalState maps a run outcome to a job state + exit code.
func terminalState(res Result, err error) (types.JobState, int) {
	switch {
	case err != nil:
		return types.JobFailed, -1
	case res.Killed:
		return types.JobKilled, res.ExitCode
	case res.ExitCode == 0:
		return types.JobSucceeded, 0
	default:
		return types.JobFailed, res.ExitCode
	}
}

func roleFor(gpus []types.GPU) types.Role {
	if len(gpus) > 0 {
		return types.RoleProvider
	}
	return types.RoleClient
}
