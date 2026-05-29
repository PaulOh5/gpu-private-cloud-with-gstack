// Package agent runs on a provider: it registers its GPUs with the
// coordinator, polls (pull) for jobs assigned to it, runs each job as a
// subprocess, batches the output back to the coordinator, and watches for a
// cancel signal.
//
// This file is the job runner — the agent's heart. It is split from the poll
// loop so it can be tested directly with shell scripts (no coordinator, no
// network):
//
//	RunJob(ctx, spec, onLogs)
//	  │  build cmd  (EnvSetup ? "sh -c '<env>; exec \"$@\"'" : direct exec)
//	  │  drop to unprivileged user if spec.RunAsUser set (provider safety)
//	  │  stream stdout+stderr ─▶ batcher (flush 100ms or 16KB) ─▶ onLogs
//	  ▼  ctx cancel ─▶ kill process ─▶ Result{Killed:true}
//	Result{ExitCode, Killed}
package agent

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// LogChunk is the shared output-batch type, aliased here for convenience within
// the agent (agent produces, coordinator relays, client consumes).
type LogChunk = types.LogChunk

// RunSpec describes one job execution.
type RunSpec struct {
	Command   []string // argv; Command[0] is the program
	EnvSetup  string   // optional shell run before Command (e.g. "source .venv/bin/activate")
	WorkDir   string   // directory to run in (the unpacked workdir tar)
	RunAsUser string   // if set and we are root, drop to this unprivileged user
}

// Result is the outcome of a job run.
type Result struct {
	ExitCode int  // remote process exit code, propagated to `flex run`
	Killed   bool // true if cancelled via context (flex cancel)
}

// Batching parameters. Per-line POST would overload the coordinator on verbose
// training logs, so chunks are flushed on an interval OR a size threshold.
const (
	flushInterval = 100 * time.Millisecond
	flushBytes    = 16 << 10 // 16 KiB
)

// RunJob executes spec.Command, calling onLogs with batched stdout/stderr.
// onLogs must be safe to call from a single goroutine (the batcher); it is
// never called concurrently. The returned Result carries the exit code (or
// Killed=true when ctx is cancelled).
func RunJob(ctx context.Context, spec RunSpec, onLogs func(LogChunk)) (Result, error) {
	if len(spec.Command) == 0 {
		return Result{}, errors.New("empty command")
	}

	cmd := buildCmd(ctx, spec)
	cmd.Dir = spec.WorkDir
	configureProcessGroup(cmd)
	if spec.RunAsUser != "" {
		if err := setRunAsUser(cmd, spec.RunAsUser); err != nil {
			return Result{}, fmt.Errorf("drop privileges to %q: %w", spec.RunAsUser, err)
		}
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{}, err
	}

	b := newBatcher(onLogs)
	defer b.close()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pump(stdout, "stdout", b) }()
	go func() { defer wg.Done(); pump(stderr, "stderr", b) }()

	if err := cmd.Start(); err != nil {
		return Result{}, fmt.Errorf("start: %w", err)
	}

	wg.Wait() // drain both pipes (they close on process exit)
	b.flush() // final partial batch
	waitErr := cmd.Wait()

	// ctx cancellation: CommandContext kills the process; report as Killed
	// rather than a misleading exit code.
	if ctx.Err() != nil {
		return Result{ExitCode: -1, Killed: true}, nil
	}
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		return Result{ExitCode: ee.ExitCode()}, nil
	}
	if waitErr != nil {
		return Result{}, waitErr
	}
	return Result{ExitCode: 0}, nil
}

// buildCmd constructs the exec.Cmd. With EnvSetup, the job runs under a shell so
// the env (venv/conda activation) is in scope; "exec \"$@\"" passes Command's
// argv positionally so no re-quoting / injection of the args is needed.
func buildCmd(ctx context.Context, spec RunSpec) *exec.Cmd {
	if spec.EnvSetup == "" {
		return exec.CommandContext(ctx, spec.Command[0], spec.Command[1:]...)
	}
	args := append([]string{"-c", spec.EnvSetup + `; exec "$@"`, "flex"}, spec.Command...)
	return exec.CommandContext(ctx, "sh", args...)
}

// pump reads r line-by-line and feeds the batcher, tagging the stream.
func pump(r io.Reader, stream string, b *batcher) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20) // tolerate long log lines
	for sc.Scan() {
		b.add(stream, sc.Text()+"\n")
	}
}

// batcher coalesces log lines and flushes on size or on an explicit flush()
// called by a ticker. It serializes onLogs behind a mutex so callers never see
// concurrent invocations.
type batcher struct {
	mu      sync.Mutex // guards bufs
	flushMu sync.Mutex // serializes flushes so onLogs is never called concurrently
	onLogs  func(LogChunk)
	bufs    map[string]*[]byte // per-stream pending bytes
	stop    chan struct{}
	stopped sync.Once
}

func newBatcher(onLogs func(LogChunk)) *batcher {
	b := &batcher{
		onLogs: onLogs,
		bufs:   map[string]*[]byte{"stdout": {}, "stderr": {}},
		stop:   make(chan struct{}),
	}
	// Ticker-driven flush so low-volume output still streams promptly.
	go func() {
		t := time.NewTicker(flushInterval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				b.flush()
			case <-b.stop:
				return
			}
		}
	}()
	return b
}

func (b *batcher) add(stream, s string) {
	b.mu.Lock()
	buf := b.bufs[stream]
	*buf = append(*buf, s...)
	over := len(*buf) >= flushBytes
	b.mu.Unlock()
	if over {
		b.flush()
	}
}

func (b *batcher) flush() {
	// flushMu serializes the emit phase so onLogs (a network POST in prod) is
	// never invoked concurrently and chunk order is preserved. mu is held only
	// to swap the buffers, so a slow onLogs does not block add().
	b.flushMu.Lock()
	defer b.flushMu.Unlock()

	b.mu.Lock()
	var out []LogChunk
	for _, stream := range []string{"stdout", "stderr"} { // stable order
		buf := b.bufs[stream]
		if len(*buf) > 0 {
			out = append(out, LogChunk{Stream: stream, Data: string(*buf)})
			*buf = (*buf)[:0]
		}
	}
	b.mu.Unlock()
	for _, c := range out {
		b.onLogs(c)
	}
}

// close stops the ticker. RunJob calls flush() before returning, so a final
// close is a tidy-up; safe to call once.
func (b *batcher) close() { b.stopped.Do(func() { close(b.stop) }) }
