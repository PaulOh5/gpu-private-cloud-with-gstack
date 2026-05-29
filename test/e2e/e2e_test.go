// Package e2e is the localhost, mock-GPU end-to-end test for flex. It is the M1
// exit criterion: it builds the real `flex` binary and drives the full path
// (coordinator + provider agent + client run) with no NVIDIA hardware and no
// tailnet — FLEX_MOCK_GPUS fakes the inventory, everything talks over
// 127.0.0.1. This exercises the CLI code (tar upload, SSE client, exit-code
// propagation) that the in-process unit/integration tests don't cover.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var flexBin string // path to the built binary, set in TestMain

func TestMain(m *testing.M) {
	if runtime.GOOS == "windows" {
		// Jobs use sh; the pool targets unix providers.
		os.Exit(0)
	}
	dir, err := os.MkdirTemp("", "flex-e2e-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, "mktemp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	flexBin = filepath.Join(dir, "flex")
	build := exec.Command("go", "build", "-o", flexBin, "./cmd/flex")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build flex: %v\n%s", err, out)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func TestEndToEndRunOnMockGPU(t *testing.T) {
	tmp := t.TempDir()
	cfg := filepath.Join(tmp, "config.json")
	coordPort := freePort(t)
	agentPort := freePort(t)
	coordURL := fmt.Sprintf("http://127.0.0.1:%d", coordPort)
	agentURL := fmt.Sprintf("http://127.0.0.1:%d", agentPort)

	baseEnv := append(os.Environ(), "FLEX_CONFIG="+cfg)

	// 1. Coordinator.
	coord := startProc(t, baseEnv, flexBin, "coordinator",
		"--db", filepath.Join(tmp, "coord.db"), "--addr", fmt.Sprintf("127.0.0.1:%d", coordPort))
	waitHTTP(t, coordURL+"/gpus")

	// 2. Client joins the coordinator.
	runCLI(t, baseEnv, "join", coordURL, "--name", "laptop")

	// 3. Provider agent with two mock GPUs.
	agentEnv := append(baseEnv, "FLEX_MOCK_GPUS=NVIDIA GeForce RTX 4090,NVIDIA RTX A6000")
	agent := startProc(t, agentEnv, flexBin, "agent",
		"--name", "office-a",
		"--listen", fmt.Sprintf("127.0.0.1:%d", agentPort),
		"--advertise", agentURL)

	// Wait until both GPUs are registered in the pool.
	waitFor(t, 5*time.Second, func() bool { return countGPUs(t, coordURL) == 2 })

	// 4. Submit a job from a working directory containing a marker file.
	proj := filepath.Join(tmp, "proj")
	mustMkdir(t, proj)
	mustWrite(t, filepath.Join(proj, "marker.txt"), "i-was-uploaded")

	out, code := runJob(t, baseEnv, proj, "run", "--gpu", "any", "--name", "demo",
		"--", "sh", "-c", "cat marker.txt; echo; echo done; exit 7")

	if code != 7 {
		t.Fatalf("flex run exit code = %d, want 7 (remote exit code not propagated)\noutput:\n%s", code, out)
	}
	if !strings.Contains(out, "i-was-uploaded") {
		t.Errorf("output missing uploaded marker file contents:\n%s", out)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("output missing command output 'done':\n%s", out)
	}

	// 5. GPU returns to the pool after the job.
	waitFor(t, 3*time.Second, func() bool { return freeGPUs(t, coordURL) == 2 })

	stopProc(agent)
	stopProc(coord)
}

// --- helpers ---

func repoRoot() string {
	wd, _ := os.Getwd() // .../test/e2e
	return filepath.Join(wd, "..", "..")
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type proc struct {
	cmd *exec.Cmd
	out *strings.Builder
}

func startProc(t *testing.T, env []string, name string, args ...string) *proc {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Env = env
	var sb strings.Builder
	cmd.Stdout = &sb
	cmd.Stderr = &sb
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
	p := &proc{cmd: cmd, out: &sb}
	t.Cleanup(func() { stopProc(p) })
	return p
}

func stopProc(p *proc) {
	if p == nil || p.cmd.Process == nil {
		return
	}
	p.cmd.Process.Kill()
	p.cmd.Wait()
}

func runCLI(t *testing.T, env []string, args ...string) {
	t.Helper()
	out, code := runCLIIn(t, env, "", args...)
	if code != 0 {
		t.Fatalf("flex %v exited %d:\n%s", args, code, out)
	}
}

func runJob(t *testing.T, env []string, dir string, args ...string) (string, int) {
	t.Helper()
	return runCLIIn(t, env, dir, args...)
}

func runCLIIn(t *testing.T, env []string, dir string, args ...string) (string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, flexBin, args...)
	cmd.Env = env
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("flex %v: %v\n%s", args, err, out)
	}
	return string(out), code
}

func waitHTTP(t *testing.T, url string) {
	t.Helper()
	waitFor(t, 5*time.Second, func() bool {
		resp, err := http.Get(url)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	})
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}

// gpuStatus mirrors the fields of store.GPUStatus we assert on (kept local to
// avoid the e2e test importing internal packages).
type gpuStatus struct {
	Free bool `json:"free"`
}

func countGPUs(t *testing.T, coordURL string) int {
	return len(getGPUs(t, coordURL))
}

func freeGPUs(t *testing.T, coordURL string) int {
	n := 0
	for _, g := range getGPUs(t, coordURL) {
		if g.Free {
			n++
		}
	}
	return n
}

func getGPUs(t *testing.T, coordURL string) []gpuStatus {
	resp, err := http.Get(coordURL + "/gpus")
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	var gpus []gpuStatus
	json.NewDecoder(resp.Body).Decode(&gpus)
	return gpus
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
