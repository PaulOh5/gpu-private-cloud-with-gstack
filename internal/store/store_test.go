package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// fixedNow is a stable timestamp so tests never depend on wall-clock.
var fixedNow = time.Unix(1_700_000_000, 0)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "flex.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func provider(t *testing.T, s *Store, id string, gpuNames ...string) {
	t.Helper()
	var gpus []types.GPU
	for i, name := range gpuNames {
		gpus = append(gpus, types.GPU{NodeID: id, Index: i, Name: name, MemTotalMB: 24000})
	}
	n := types.Node{ID: id, Addr: id + ".tailnet", Role: types.RoleProvider, GPUs: gpus, LastHeartbeat: fixedNow}
	if err := s.RegisterNode(context.Background(), n); err != nil {
		t.Fatalf("RegisterNode(%s): %v", id, err)
	}
}

func queueJob(t *testing.T, s *Store, id string, spec types.GPUSpec) {
	t.Helper()
	j := types.Job{ID: id, Name: id, User: "u", Command: []string{"true"}, Spec: spec, State: types.JobQueued, CreatedAt: fixedNow, UpdatedAt: fixedNow}
	if err := s.SubmitJob(context.Background(), j); err != nil {
		t.Fatalf("SubmitJob(%s): %v", id, err)
	}
}

func TestRegisterAndListGPUs(t *testing.T) {
	s := openTest(t)
	provider(t, s, "office-a", "NVIDIA GeForce RTX 4090", "NVIDIA RTX A6000")
	gpus, err := s.ListGPUs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gpus) != 2 {
		t.Fatalf("got %d gpus, want 2", len(gpus))
	}
	for _, g := range gpus {
		if !g.Free {
			t.Errorf("gpu %s should be free initially", g.GlobalID())
		}
		if g.NodeAddr != "office-a.tailnet" {
			t.Errorf("gpu %s addr = %q", g.GlobalID(), g.NodeAddr)
		}
	}
}

func TestAssignBasic(t *testing.T) {
	s := openTest(t)
	provider(t, s, "office-a", "NVIDIA GeForce RTX 4090")
	queueJob(t, s, "job1", types.GPUSpec{AnyModel: true, Count: 1})

	job, err := s.Assign(context.Background(), "job1", fixedNow, time.Minute)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if job.State != types.JobAssigned || job.NodeID != "office-a" || len(job.GPUIndexes) != 1 {
		t.Fatalf("assigned job = %+v", job)
	}
	// GPU must now read busy in the ledger.
	gpus, _ := s.ListGPUs(context.Background())
	if gpus[0].Free || gpus[0].AssignedJob != "job1" {
		t.Errorf("gpu should be busy with job1, got %+v", gpus[0])
	}
}

func TestAssignNoFreeGPUWhenModelMismatch(t *testing.T) {
	s := openTest(t)
	provider(t, s, "office-a", "NVIDIA GeForce RTX 4090")
	queueJob(t, s, "job1", types.GPUSpec{Model: "a6000", Count: 1})
	_, err := s.Assign(context.Background(), "job1", fixedNow, time.Minute)
	if !errors.Is(err, ErrNoFreeGPU) {
		t.Fatalf("Assign err = %v, want ErrNoFreeGPU", err)
	}
}

func TestAssignSkipsStaleProvider(t *testing.T) {
	s := openTest(t)
	provider(t, s, "office-a", "NVIDIA GeForce RTX 4090")
	queueJob(t, s, "job1", types.GPUSpec{AnyModel: true, Count: 1})
	// now is 10 minutes after the provider's last heartbeat; liveWithin=1m.
	_, err := s.Assign(context.Background(), "job1", fixedNow.Add(10*time.Minute), time.Minute)
	if !errors.Is(err, ErrNoFreeGPU) {
		t.Fatalf("Assign err = %v, want ErrNoFreeGPU (provider stale)", err)
	}
}

// TestAssignLastGPURace is the CRITICAL correctness test: with exactly one free
// GPU and two queued jobs, two concurrent Assign calls must result in exactly
// one success and one ErrNoFreeGPU — never a double-assignment. This is what
// BEGIN IMMEDIATE buys us.
func TestAssignLastGPURace(t *testing.T) {
	s := openTest(t)
	provider(t, s, "office-a", "NVIDIA GeForce RTX 4090") // 1 GPU
	queueJob(t, s, "jobA", types.GPUSpec{AnyModel: true, Count: 1})
	queueJob(t, s, "jobB", types.GPUSpec{AnyModel: true, Count: 1})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	ids := []string{"jobA", "jobB"}
	start := make(chan struct{})
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release both goroutines as simultaneously as possible
			_, errs[i] = s.Assign(context.Background(), ids[i], fixedNow, time.Minute)
		}(i)
	}
	close(start)
	wg.Wait()

	var wins, noGPU int
	for _, e := range errs {
		switch {
		case e == nil:
			wins++
		case errors.Is(e, ErrNoFreeGPU):
			noGPU++
		default:
			t.Fatalf("unexpected Assign error: %v", e)
		}
	}
	if wins != 1 || noGPU != 1 {
		t.Fatalf("race: wins=%d noGPU=%d, want exactly 1 and 1 (double-assignment bug?)", wins, noGPU)
	}
	// Ledger must show exactly one busy GPU.
	gpus, _ := s.ListGPUs(context.Background())
	if gpus[0].Free {
		t.Fatal("the one GPU should be busy after the race")
	}
}

func TestReRegisterPreservesAssignment(t *testing.T) {
	s := openTest(t)
	provider(t, s, "office-a", "NVIDIA GeForce RTX 4090")
	queueJob(t, s, "job1", types.GPUSpec{AnyModel: true, Count: 1})
	if _, err := s.Assign(context.Background(), "job1", fixedNow, time.Minute); err != nil {
		t.Fatal(err)
	}
	// Agent restarts and re-registers the same GPU (heartbeat refreshed).
	provider(t, s, "office-a", "NVIDIA GeForce RTX 4090")
	gpus, _ := s.ListGPUs(context.Background())
	if gpus[0].Free || gpus[0].AssignedJob != "job1" {
		t.Fatalf("re-register must not free a running GPU; got %+v", gpus[0])
	}
}

func TestMultiGPUJobLandsOnOneNode(t *testing.T) {
	s := openTest(t)
	provider(t, s, "office-a", "NVIDIA GeForce RTX 4090", "NVIDIA GeForce RTX 4090")
	provider(t, s, "rig-home", "NVIDIA GeForce RTX 3090") // only 1 GPU, can't satisfy count:2
	queueJob(t, s, "job1", types.GPUSpec{AnyModel: true, Count: 2})

	job, err := s.Assign(context.Background(), "job1", fixedNow, time.Minute)
	if err != nil {
		t.Fatalf("Assign: %v", err)
	}
	if job.NodeID != "office-a" || len(job.GPUIndexes) != 2 {
		t.Fatalf("count:2 job should take both GPUs on office-a, got node=%s idxs=%v", job.NodeID, job.GPUIndexes)
	}
}
