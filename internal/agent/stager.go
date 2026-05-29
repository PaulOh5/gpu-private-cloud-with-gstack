package agent

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/workdir"
)

// Stager resolves the local directory holding a job's code. The agent only
// starts a job once its Stager reports the workdir ready, so a job never runs
// before its client→agent tar upload has landed.
type Stager interface {
	// Dir returns the staged directory for jobID and whether it is ready.
	Dir(jobID string) (dir string, ready bool)
	// Cleanup removes a finished job's staged directory. Called by the agent
	// once the job reaches a terminal state so workdirs don't pile up on disk.
	Cleanup(jobID string)
}

// WorkdirStager receives client tar uploads (data plane, P2P) and unpacks each
// into its own directory under Base. It satisfies Stager.
type WorkdirStager struct {
	Base string // parent dir for per-job workdirs

	mu   sync.Mutex
	dirs map[string]string // jobID -> staged dir (presence = ready)
}

// NewWorkdirStager creates a stager rooted at base (created if missing).
func NewWorkdirStager(base string) (*WorkdirStager, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, err
	}
	return &WorkdirStager{Base: base, dirs: map[string]string{}}, nil
}

// Dir implements Stager.
func (s *WorkdirStager) Dir(jobID string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.dirs[jobID]
	return d, ok
}

// Cleanup removes a finished job's staged directory.
func (s *WorkdirStager) Cleanup(jobID string) {
	s.mu.Lock()
	d := s.dirs[jobID]
	delete(s.dirs, jobID)
	s.mu.Unlock()
	if d != "" {
		os.RemoveAll(d)
	}
}

// Handler serves POST /workdir/{jobID}: the client uploads a gzip tar of its
// working directory, which is unpacked into a fresh per-job dir. Re-upload
// replaces the previous staging.
func (s *WorkdirStager) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /workdir/{jobID}", func(w http.ResponseWriter, r *http.Request) {
		jobID := r.PathValue("jobID")
		dir, err := os.MkdirTemp(s.Base, sanitize(jobID)+"-")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := workdir.Untar(r.Body, dir); err != nil {
			os.RemoveAll(dir)
			http.Error(w, "untar: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.mu.Lock()
		old := s.dirs[jobID] // a re-upload replaces the prior staging
		s.dirs[jobID] = dir
		s.mu.Unlock()
		if old != "" {
			os.RemoveAll(old)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// sanitize keeps a job id usable as a path prefix.
func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return filepath.Base(string(out))
}
