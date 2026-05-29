package agent

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// fakeControl is an in-process Control for testing the agent loop without HTTP.
type fakeControl struct {
	mu         sync.Mutex
	registered *types.Node
	heartbeats int
	assigned   []types.Job // returned once, then cleared (avoid re-starting)
	cancels    []string
	statuses   []statusUpdate
	logs       map[string]string
}

type statusUpdate struct {
	jobID    string
	state    types.JobState
	exitCode int
}

func newFake() *fakeControl { return &fakeControl{logs: map[string]string{}} }

func (f *fakeControl) Register(_ context.Context, n types.Node) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = &n
	return nil
}
func (f *fakeControl) Heartbeat(context.Context, string) error {
	f.mu.Lock()
	f.heartbeats++
	f.mu.Unlock()
	return nil
}
func (f *fakeControl) PollAssigned(context.Context, string) ([]types.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.assigned
	f.assigned = nil
	return out, nil
}
func (f *fakeControl) PollCancels(context.Context, string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.cancels
	f.cancels = nil
	return out, nil
}
func (f *fakeControl) ReportStatus(_ context.Context, jobID string, st types.JobState, code int) error {
	f.mu.Lock()
	f.statuses = append(f.statuses, statusUpdate{jobID, st, code})
	f.mu.Unlock()
	return nil
}
func (f *fakeControl) AppendLogs(_ context.Context, jobID string, chunks []LogChunk) error {
	f.mu.Lock()
	for _, c := range chunks {
		f.logs[jobID] += c.Data
	}
	f.mu.Unlock()
	return nil
}

func (f *fakeControl) lastStatus(jobID string) (statusUpdate, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.statuses) - 1; i >= 0; i-- {
		if f.statuses[i].jobID == jobID {
			return f.statuses[i], true
		}
	}
	return statusUpdate{}, false
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func TestAgentRegistersWithGPUs(t *testing.T) {
	f := newFake()
	a := &Agent{NodeID: "office-a", Addr: "office-a.tailnet", Control: f, Interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, []types.GPU{{NodeID: "office-a", Index: 0, Name: "RTX 4090"}})

	waitFor(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.registered != nil && f.heartbeats > 0
	})
	if f.registered.Role != types.RoleProvider {
		t.Errorf("role = %s, want provider", f.registered.Role)
	}
}

func TestAgentRunsAssignedJobAndReportsSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := newFake()
	f.assigned = []types.Job{{
		ID: "job1", Command: []string{"sh", "-c", "echo hello"}, State: types.JobAssigned,
	}}
	a := &Agent{NodeID: "office-a", Control: f, Interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, []types.GPU{{Index: 0, Name: "RTX 4090"}})

	waitFor(t, func() bool {
		s, ok := f.lastStatus("job1")
		return ok && s.state == types.JobSucceeded
	})
	s, _ := f.lastStatus("job1")
	if s.exitCode != 0 {
		t.Errorf("exit = %d, want 0", s.exitCode)
	}
	f.mu.Lock()
	got := f.logs["job1"]
	f.mu.Unlock()
	if got == "" || got[:5] != "hello" {
		t.Errorf("logs = %q, want to start with 'hello'", got)
	}
}

func TestAgentReportsFailureExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := newFake()
	f.assigned = []types.Job{{ID: "jobX", Command: []string{"sh", "-c", "exit 7"}, State: types.JobAssigned}}
	a := &Agent{NodeID: "n", Control: f, Interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, []types.GPU{{Index: 0}})

	waitFor(t, func() bool {
		s, ok := f.lastStatus("jobX")
		return ok && s.state == types.JobFailed
	})
	s, _ := f.lastStatus("jobX")
	if s.exitCode != 7 {
		t.Errorf("exit = %d, want 7", s.exitCode)
	}
}

func TestAgentCancelsRunningJob(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh")
	}
	f := newFake()
	f.assigned = []types.Job{{ID: "sleeper", Command: []string{"sh", "-c", "sleep 30"}, State: types.JobAssigned}}
	a := &Agent{NodeID: "n", Control: f, Interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, []types.GPU{{Index: 0}})

	// Wait until it's running, then request cancellation.
	waitFor(t, func() bool {
		s, ok := f.lastStatus("sleeper")
		return ok && s.state == types.JobRunning
	})
	f.mu.Lock()
	f.cancels = []string{"sleeper"}
	f.mu.Unlock()

	waitFor(t, func() bool {
		s, ok := f.lastStatus("sleeper")
		return ok && s.state == types.JobKilled
	})
}
