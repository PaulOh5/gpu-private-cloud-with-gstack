package agent

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// collector is a concurrency-safe onLogs sink that also asserts onLogs is never
// called concurrently (the batcher contract).
type collector struct {
	mu       sync.Mutex
	inFlight int
	stdout   strings.Builder
	stderr   strings.Builder
	raced    bool
}

func (c *collector) onLogs(ch LogChunk) {
	c.mu.Lock()
	if c.inFlight != 0 {
		c.raced = true
	}
	c.inFlight++
	c.mu.Unlock()

	time.Sleep(time.Millisecond) // widen the window for a concurrency bug to show

	c.mu.Lock()
	c.inFlight--
	switch ch.Stream {
	case "stdout":
		c.stdout.WriteString(ch.Data)
	case "stderr":
		c.stderr.WriteString(ch.Data)
	}
	c.mu.Unlock()
}

func skipNonUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runner tests use sh; skip on windows")
	}
}

func TestRunJobExitCode(t *testing.T) {
	skipNonUnix(t)
	c := &collector{}
	res, err := RunJob(context.Background(), RunSpec{Command: []string{"sh", "-c", "exit 3"}}, c.onLogs)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if res.ExitCode != 3 || res.Killed {
		t.Fatalf("Result = %+v, want exit 3 not killed", res)
	}
}

func TestRunJobCapturesStdoutStderr(t *testing.T) {
	skipNonUnix(t)
	c := &collector{}
	res, err := RunJob(context.Background(),
		RunSpec{Command: []string{"sh", "-c", "echo out; echo err 1>&2"}}, c.onLogs)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(c.stdout.String(), "out") {
		t.Errorf("stdout = %q, want to contain 'out'", c.stdout.String())
	}
	if !strings.Contains(c.stderr.String(), "err") {
		t.Errorf("stderr = %q, want to contain 'err'", c.stderr.String())
	}
	if c.raced {
		t.Error("onLogs was called concurrently — batcher must serialize emits")
	}
}

func TestRunJobEnvSetup(t *testing.T) {
	skipNonUnix(t)
	c := &collector{}
	// EnvSetup sets a var; Command echoes it. Proves the env is in scope and
	// the command's args are passed through exec "$@" intact.
	_, err := RunJob(context.Background(), RunSpec{
		EnvSetup: "export GREETING=hello",
		Command:  []string{"sh", "-c", `echo "$GREETING world"`},
	}, c.onLogs)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if got := strings.TrimSpace(c.stdout.String()); got != "hello world" {
		t.Errorf("stdout = %q, want 'hello world'", got)
	}
}

func TestRunJobCancel(t *testing.T) {
	skipNonUnix(t)
	c := &collector{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	res, err := RunJob(ctx, RunSpec{Command: []string{"sh", "-c", "sleep 30"}}, c.onLogs)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	if !res.Killed {
		t.Fatalf("Result = %+v, want Killed=true", res)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("cancel did not promptly kill the process")
	}
}

func TestRunJobEmptyCommand(t *testing.T) {
	_, err := RunJob(context.Background(), RunSpec{}, func(LogChunk) {})
	if err == nil {
		t.Fatal("empty command should error")
	}
}

func TestRunJobWorkDir(t *testing.T) {
	skipNonUnix(t)
	dir := t.TempDir()
	c := &collector{}
	_, err := RunJob(context.Background(), RunSpec{WorkDir: dir, Command: []string{"pwd"}}, c.onLogs)
	if err != nil {
		t.Fatalf("RunJob: %v", err)
	}
	// macOS /tmp symlinks to /private/tmp; just require the temp dir's leaf.
	if !strings.Contains(c.stdout.String(), dirLeaf(dir)) {
		t.Errorf("pwd = %q, want to contain workdir leaf %q", c.stdout.String(), dirLeaf(dir))
	}
}

func dirLeaf(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
