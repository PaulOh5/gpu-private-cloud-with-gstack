// Package coordinator is the HTTP/JSON control plane of the pool. It is a thin
// shell over store: every endpoint validates input and delegates the real work
// to the Store, so scheduling correctness lives in one tested place.
//
// Transport is pull-based and HTTP/JSON (curl-debuggable): agents POST
// /register + /heartbeat, clients GET /gpus and POST /jobs. The coordinator
// never pushes to agents and never carries the workdir payload (that is
// client→agent P2P) — it only moves control state.
//
//	provider agent ──POST /register──▶┐
//	               ──POST /heartbeat─▶│  coordinator  ──▶ store (SQLite/WAL)
//	client         ──GET  /gpus──────▶│   (this pkg)        assignment ledger
//	               ──POST /jobs──────▶┘
package coordinator

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/store"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// Coordinator holds the dependencies shared by the handlers.
type Coordinator struct {
	store      *store.Store
	now        func() time.Time
	liveWithin time.Duration // a provider missing heartbeats this long is not assignable
}

// Option configures a Coordinator.
type Option func(*Coordinator)

// WithClock overrides the time source (tests inject a fixed clock).
func WithClock(now func() time.Time) Option { return func(c *Coordinator) { c.now = now } }

// WithLiveWithin sets how long a provider may go without a heartbeat before its
// GPUs stop being assignable.
func WithLiveWithin(d time.Duration) Option { return func(c *Coordinator) { c.liveWithin = d } }

// New builds a Coordinator and returns it together with its HTTP handler.
func New(s *store.Store, opts ...Option) (*Coordinator, http.Handler) {
	c := &Coordinator{store: s, now: time.Now, liveWithin: 30 * time.Second}
	for _, o := range opts {
		o(c)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", c.handleRegister)
	mux.HandleFunc("POST /heartbeat", c.handleHeartbeat)
	mux.HandleFunc("GET /gpus", c.handleGPUs)
	mux.HandleFunc("POST /jobs", c.handleSubmit)
	mux.HandleFunc("GET /jobs/{id}", c.handleGetJob)
	return c, mux
}

func (c *Coordinator) handleRegister(w http.ResponseWriter, r *http.Request) {
	var n types.Node
	if !decode(w, r, &n) {
		return
	}
	if n.ID == "" {
		writeErr(w, http.StatusBadRequest, "node id required")
		return
	}
	if n.LastHeartbeat.IsZero() {
		n.LastHeartbeat = c.now()
	}
	if n.Role == "" {
		if len(n.GPUs) > 0 {
			n.Role = types.RoleProvider
		} else {
			n.Role = types.RoleClient
		}
	}
	if err := c.store.RegisterNode(r.Context(), n); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "registered", "node": n.ID})
}

func (c *Coordinator) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		NodeID string `json:"node_id"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.NodeID == "" {
		writeErr(w, http.StatusBadRequest, "node_id required")
		return
	}
	if err := c.store.Heartbeat(r.Context(), body.NodeID, c.now()); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (c *Coordinator) handleGPUs(w http.ResponseWriter, r *http.Request) {
	gpus, err := c.store.ListGPUs(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if gpus == nil {
		gpus = []store.GPUStatus{}
	}
	writeJSON(w, http.StatusOK, gpus)
}

// handleSubmit accepts a job, persists it queued, then attempts an immediate
// assignment. If no GPU is free the job stays queued (the response says so);
// the M2 scheduler retries queued jobs as GPUs free up.
func (c *Coordinator) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var j types.Job
	if !decode(w, r, &j) {
		return
	}
	if j.ID == "" || len(j.Command) == 0 {
		writeErr(w, http.StatusBadRequest, "job id and command required")
		return
	}
	if j.Spec.Count < 1 {
		j.Spec.Count = 1
	}
	now := c.now()
	j.State = types.JobQueued
	j.CreatedAt, j.UpdatedAt = now, now
	if err := c.store.SubmitJob(r.Context(), j); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	assigned, err := c.store.Assign(r.Context(), j.ID, now, c.liveWithin)
	switch {
	case err == nil:
		writeJSON(w, http.StatusCreated, assigned)
	case errors.Is(err, store.ErrNoFreeGPU):
		j.State = types.JobQueued
		writeJSON(w, http.StatusAccepted, j) // 202: accepted, waiting for a GPU
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

func (c *Coordinator) handleGetJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := c.store.GetJob(r.Context(), id)
	if errors.Is(err, store.ErrJobNotFound) {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// decode reads a JSON body into v, writing a 400 and returning false on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
