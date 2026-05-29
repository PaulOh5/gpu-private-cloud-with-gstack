package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/agent"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/coordinator"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/store"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// TestAgentCoordinatorRoundTrip wires a real Agent to a real coordinator over
// HTTP and runs a job end to end: register a provider GPU, submit a job (which
// the coordinator assigns to that provider), let the agent pull + run it, then
// confirm the coordinator sees it succeeded and the logs streamed back. This is
// the M1 wiring proof that T7's mock-GPU E2E builds on.
func TestAgentCoordinatorRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("job uses sh")
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "flex.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_, handler := coordinator.New(st, coordinator.WithLiveWithin(time.Minute))
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	ctrl := agent.NewHTTPControl(srv.URL)

	// Start the provider agent with one GPU.
	a := &agent.Agent{NodeID: "office-a", Addr: "office-a.tailnet", Control: ctrl, Interval: 5 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, []types.GPU{{NodeID: "office-a", Index: 0, Name: "NVIDIA GeForce RTX 4090", MemTotalMB: 24000}})

	// Wait for the provider's GPU to show up in the pool.
	waitFor(t, 3*time.Second, func() bool {
		var gpus []store.GPUStatus
		getJSON(t, srv.URL+"/gpus", &gpus)
		return len(gpus) == 1
	})

	// A client submits a job; the coordinator assigns it to office-a.
	job := types.Job{ID: "job1", Name: "demo", Command: []string{"sh", "-c", "echo trained"}, Spec: types.GPUSpec{AnyModel: true, Count: 1}}
	postJSON(t, srv.URL+"/jobs", job)

	// The agent should run it and report success.
	waitFor(t, 5*time.Second, func() bool {
		var j types.Job
		getJSON(t, srv.URL+"/jobs/job1", &j)
		return j.State == types.JobSucceeded
	})

	var j types.Job
	getJSON(t, srv.URL+"/jobs/job1", &j)
	if j.ExitCode != 0 || j.NodeID != "office-a" {
		t.Fatalf("job = %+v", j)
	}

	// GPU should be free again after the job finished.
	var gpus []store.GPUStatus
	getJSON(t, srv.URL+"/gpus", &gpus)
	if !gpus[0].Free {
		t.Error("GPU should be free after job completion")
	}

	// Logs should have streamed to the hub (backlog visible via SSE).
	logs := readSSELogs(t, srv.URL+"/jobs/job1/logs/stream")
	if !strings.Contains(logs, "trained") {
		t.Errorf("streamed logs = %q, want to contain 'trained'", logs)
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(out)
}

func postJSON(t *testing.T, url string, in any) {
	t.Helper()
	b, _ := json.Marshal(in)
	resp, err := http.Post(url, "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
}

// readSSELogs reads the log stream until the "done" event (job already
// finished, so the stream replays backlog then closes).
func readSSELogs(t *testing.T, url string) string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if strings.Contains(sb.String(), "event: done") || err != nil {
			break
		}
	}
	return sb.String()
}
