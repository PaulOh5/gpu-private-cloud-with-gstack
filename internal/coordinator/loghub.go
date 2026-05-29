package coordinator

import (
	"sync"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// logHub fans out job log chunks from the agent (which POSTs them) to any
// connected clients (which read them over SSE). Logs live in memory only —
// SQLite is unfit for high-frequency appends (design decision) — so each job
// keeps a bounded backlog ring so a client that attaches late, or reattaches
// after `flex logs`, still sees recent output.
//
//	agent ──POST /jobs/{id}/logs──▶ logHub.append ──▶ ring (backlog)
//	                                              └──▶ live subscribers (SSE)
//	agent ──POST /jobs/{id}/status (terminal)──▶ logHub.finish ──▶ close streams
type logHub struct {
	mu      sync.Mutex
	jobs    map[string]*jobLog
	backlog int // max chunks retained per job for late/reattaching subscribers
}

type jobLog struct {
	backlog []types.LogChunk
	subs    map[chan types.LogChunk]struct{}
	done    bool
	exit    int
	state   types.JobState
}

func newLogHub(backlog int) *logHub {
	if backlog <= 0 {
		backlog = 1024
	}
	return &logHub{jobs: map[string]*jobLog{}, backlog: backlog}
}

func (h *logHub) jobLocked(id string) *jobLog {
	jl := h.jobs[id]
	if jl == nil {
		jl = &jobLog{subs: map[chan types.LogChunk]struct{}{}}
		h.jobs[id] = jl
	}
	return jl
}

// append records chunks and fans them out to live subscribers.
func (h *logHub) append(id string, chunks []types.LogChunk) {
	h.mu.Lock()
	defer h.mu.Unlock()
	jl := h.jobLocked(id)
	for _, c := range chunks {
		jl.backlog = append(jl.backlog, c)
		if len(jl.backlog) > h.backlog {
			jl.backlog = jl.backlog[len(jl.backlog)-h.backlog:]
		}
		for ch := range jl.subs {
			// Non-blocking: a stalled client must not back up the agent's POST.
			select {
			case ch <- c:
			default:
			}
		}
	}
}

// finish marks a job terminal and closes all subscriber channels so their SSE
// streams end. Late subscribers (after finish) get the backlog then an
// immediate close.
func (h *logHub) finish(id string, state types.JobState, exit int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	jl := h.jobLocked(id)
	jl.done = true
	jl.state = state
	jl.exit = exit
	for ch := range jl.subs {
		close(ch)
		delete(jl.subs, ch)
	}
}

// subscription is a client's view of a job's logs: the backlog already
// captured, a live channel for new chunks (closed when the job finishes), and
// the terminal status if the job is already done.
type subscription struct {
	Backlog []types.LogChunk
	Live    <-chan types.LogChunk
	Done    bool
	State   types.JobState
	Exit    int
	cancel  func()
}

// subscribe returns the current backlog plus a live channel. If the job is
// already finished, Done is true, Live is a closed channel, and Backlog holds
// everything that was produced.
func (h *logHub) subscribe(id string) *subscription {
	h.mu.Lock()
	defer h.mu.Unlock()
	jl := h.jobLocked(id)

	backlog := make([]types.LogChunk, len(jl.backlog))
	copy(backlog, jl.backlog)

	if jl.done {
		closed := make(chan types.LogChunk)
		close(closed)
		return &subscription{Backlog: backlog, Live: closed, Done: true, State: jl.state, Exit: jl.exit, cancel: func() {}}
	}

	ch := make(chan types.LogChunk, 256)
	jl.subs[ch] = struct{}{}
	cancel := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := jl.subs[ch]; ok {
			delete(jl.subs, ch)
			close(ch)
		}
	}
	return &subscription{Backlog: backlog, Live: ch, cancel: cancel}
}

// status reports a job's terminal status from the hub's view (for clients that
// only want the final exit code).
func (h *logHub) status(id string) (done bool, state types.JobState, exit int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	jl := h.jobs[id]
	if jl == nil {
		return false, "", 0
	}
	return jl.done, jl.state, jl.exit
}
