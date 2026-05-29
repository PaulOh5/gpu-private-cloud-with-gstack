package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/store"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

var fixedNow = time.Unix(1_700_000_000, 0)

func newServer(t *testing.T) *httptest.Server {
	srv, _ := newServerWithStore(t)
	return srv
}

func newServerWithStore(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "flex.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	_, h := New(s, WithClock(func() time.Time { return fixedNow }), WithLiveWithin(time.Minute))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, s
}

func post(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// TestRegisterGpusAndAssignFlow walks the M1 happy path through HTTP:
// register a provider, see its GPU via /gpus, submit a job, get it assigned.
func TestRegisterGpusAndAssignFlow(t *testing.T) {
	srv := newServer(t)

	node := types.Node{
		ID: "office-a", Addr: "office-a.tailnet", Role: types.RoleProvider,
		GPUs: []types.GPU{{NodeID: "office-a", Index: 0, Name: "NVIDIA GeForce RTX 4090", MemTotalMB: 24000}},
	}
	if resp := post(t, srv.URL+"/register", node); resp.StatusCode != http.StatusOK {
		t.Fatalf("/register status = %d", resp.StatusCode)
	}

	// /gpus shows one free GPU.
	resp, err := http.Get(srv.URL + "/gpus")
	if err != nil {
		t.Fatal(err)
	}
	var gpus []store.GPUStatus
	json.NewDecoder(resp.Body).Decode(&gpus)
	if len(gpus) != 1 || !gpus[0].Free {
		t.Fatalf("/gpus = %+v, want 1 free GPU", gpus)
	}

	// Submit a job → assigned (201) on the free GPU.
	job := types.Job{ID: "job1", Name: "t", Command: []string{"python", "train.py"}, Spec: types.GPUSpec{AnyModel: true, Count: 1}}
	resp = post(t, srv.URL+"/jobs", job)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("/jobs status = %d, want 201", resp.StatusCode)
	}
	var assigned types.Job
	json.NewDecoder(resp.Body).Decode(&assigned)
	if assigned.State != types.JobAssigned || assigned.NodeID != "office-a" {
		t.Fatalf("assigned job = %+v", assigned)
	}

	// /gpus now shows the GPU busy.
	resp, _ = http.Get(srv.URL + "/gpus")
	json.NewDecoder(resp.Body).Decode(&gpus)
	if gpus[0].Free {
		t.Error("GPU should be busy after assignment")
	}
}

// TestSubmitQueuesWhenNoGPU: with no providers, a job is accepted (202) and
// stays queued rather than erroring.
func TestSubmitQueuesWhenNoGPU(t *testing.T) {
	srv := newServer(t)
	job := types.Job{ID: "job1", Command: []string{"true"}, Spec: types.GPUSpec{AnyModel: true, Count: 1}}
	resp := post(t, srv.URL+"/jobs", job)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("/jobs status = %d, want 202 (queued)", resp.StatusCode)
	}
	var j types.Job
	json.NewDecoder(resp.Body).Decode(&j)
	if j.State != types.JobQueued {
		t.Fatalf("job state = %s, want queued", j.State)
	}
}

func TestHeartbeatUnknownNode(t *testing.T) {
	srv := newServer(t)
	resp := post(t, srv.URL+"/heartbeat", map[string]string{"node_id": "ghost"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/heartbeat unknown node = %d, want 404", resp.StatusCode)
	}
}

func TestSubmitRejectsMissingFields(t *testing.T) {
	srv := newServer(t)
	resp := post(t, srv.URL+"/jobs", types.Job{ID: "x"}) // no command
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestGetJobNotFound(t *testing.T) {
	srv := newServer(t)
	resp, err := http.Get(srv.URL + "/jobs/nope")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestCancelAssignedFreesGPU: cancelling a job that is assigned but not yet
// running on an agent must kill it directly and return the GPU to the pool —
// otherwise a pre-start cancel strands the reserved GPU (adversarial #11).
func TestCancelAssignedFreesGPU(t *testing.T) {
	srv := newServer(t)
	node := types.Node{ID: "office-a", Addr: "office-a.tailnet", Role: types.RoleProvider,
		GPUs: []types.GPU{{NodeID: "office-a", Index: 0, Name: "RTX 4090", MemTotalMB: 24000}}}
	post(t, srv.URL+"/register", node)

	job := types.Job{ID: "job1", Command: []string{"true"}, Spec: types.GPUSpec{AnyModel: true, Count: 1}}
	if resp := post(t, srv.URL+"/jobs", job); resp.StatusCode != http.StatusCreated {
		t.Fatalf("/jobs = %d, want 201 assigned", resp.StatusCode)
	}
	// Cancel before any agent runs it.
	if resp := post(t, srv.URL+"/jobs/job1/cancel", struct{}{}); resp.StatusCode != http.StatusOK {
		t.Fatalf("/cancel = %d, want 200", resp.StatusCode)
	}
	// GPU must be free again.
	resp, _ := http.Get(srv.URL + "/gpus")
	var gpus []store.GPUStatus
	json.NewDecoder(resp.Body).Decode(&gpus)
	if len(gpus) != 1 || !gpus[0].Free {
		t.Fatalf("GPU should be free after pre-start cancel, got %+v", gpus)
	}
	var j types.Job
	getJSON(t, srv.URL+"/jobs/job1", &j)
	if j.State != types.JobKilled {
		t.Fatalf("job state = %s, want killed", j.State)
	}
}

// TestLogStreamTerminalNoHang: streaming logs for a job that is terminal in the
// store but unknown to the in-memory hub (e.g. after a coordinator restart)
// must return a done event, not block forever (adversarial #2).
func TestLogStreamTerminalNoHang(t *testing.T) {
	srv, st := newServerWithStore(t)
	// Seed a terminal job directly in the store — the hub never saw it.
	j := types.Job{ID: "ghost", Command: []string{"true"}, Spec: types.GPUSpec{AnyModel: true, Count: 1},
		State: types.JobQueued, CreatedAt: fixedNow, UpdatedAt: fixedNow}
	if err := st.SubmitJob(context.Background(), j); err != nil {
		t.Fatal(err)
	}
	if err := st.SetJobStatus(context.Background(), "ghost", types.JobSucceeded, 0, fixedNow); err != nil {
		t.Fatal(err)
	}

	// The stream must complete quickly with a done event.
	done := make(chan string, 1)
	go func() {
		resp, err := http.Get(srv.URL + "/jobs/ghost/logs/stream")
		if err != nil {
			done <- "error: " + err.Error()
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		done <- string(b)
	}()
	select {
	case body := <-done:
		if !strings.Contains(body, "event: done") {
			t.Fatalf("stream body missing done event: %q", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("log stream hung on a terminal job (should have returned done)")
	}
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(out)
}
