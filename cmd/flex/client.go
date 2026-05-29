package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/user"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/config"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/store"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/workdir"
)

// runJoin points this machine at a coordinator (the explicit bootstrap step).
func runJoin(args []string) error {
	fs := flag.NewFlagSet("join", flag.ExitOnError)
	name := fs.String("name", "", "node id for this machine (default: hostname)")
	// Go's flag package stops at the first positional, so pull the URL out of
	// args first — this lets `flex join <url> --name x` (the natural order) work
	// as well as `flex join --name x <url>`.
	url, rest := extractFirstPositional(args)
	fs.Parse(rest)
	if url == "" {
		return errors.New("usage: flex join <coordinator-url> [--name NODE]")
	}
	url = strings.TrimRight(url, "/")
	node := firstNonEmpty(*name, hostname())
	if err := config.Save(config.Config{Coordinator: url, Node: node}); err != nil {
		return err
	}
	fmt.Printf("joined %s as %q\n", url, node)
	return nil
}

func mustConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err != nil {
		return cfg, err
	}
	if cfg.Coordinator == "" {
		return cfg, errors.New("no coordinator configured; run `flex join <coordinator-url>` first")
	}
	return cfg, nil
}

// extractFirstPositional returns the first non-flag arg and the remaining args
// (with that one removed), so a positional can precede flags.
func extractFirstPositional(args []string) (string, []string) {
	pos := ""
	rest := make([]string, 0, len(args))
	for _, a := range args {
		if pos == "" && !strings.HasPrefix(a, "-") {
			pos = a
			continue
		}
		rest = append(rest, a)
	}
	return pos, rest
}

// runGPUs lists the pool's GPUs as a table.
func runGPUs(args []string) error {
	cfg, err := mustConfig()
	if err != nil {
		return err
	}
	var gpus []store.GPUStatus
	if err := httpGetJSON(cfg.Coordinator+"/gpus", &gpus); err != nil {
		return err
	}
	if len(gpus) == 0 {
		fmt.Println("no GPUs in the pool yet (start providers with `flex agent`)")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tGPU\tMODEL\tVRAM(GB)\tSTATUS")
	for _, g := range gpus {
		status := "idle"
		if !g.Free {
			status = "busy:" + g.AssignedJob
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%d\t%s\n", g.NodeID, g.Index, g.Name, g.MemTotalMB/1024, status)
	}
	return tw.Flush()
}

// runRun submits a job, uploads the working directory to the assigned agent,
// then streams logs and exits with the remote command's exit code.
func runRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	gpu := fs.String("gpu", "any", "GPU spec: any | MODEL | count:N")
	vram := fs.Int("vram", 0, "minimum VRAM in GB")
	name := fs.String("name", "", "job name")
	env := fs.String("env", "", "shell setup run before the command (e.g. 'source .venv/bin/activate')")
	detach := fs.Bool("detach", false, "submit and exit without streaming logs")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("usage: flex run [flags] -- COMMAND [ARGS...]")
	}
	cfg, err := mustConfig()
	if err != nil {
		return err
	}
	spec, err := types.ParseGPUSpec(*gpu)
	if err != nil {
		return err
	}

	jobID := newID()
	job := types.Job{
		ID:        jobID,
		Name:      firstNonEmpty(*name, fs.Arg(0)),
		User:      currentUser(),
		Command:   fs.Args(),
		EnvSetup:  *env,
		Spec:      spec,
		VRAMMinMB: *vram * 1024,
	}

	var submitted types.Job
	status, err := httpPostJSON(cfg.Coordinator+"/jobs", job, &submitted)
	if err != nil {
		return err
	}
	if status == http.StatusAccepted {
		// Queued: no free GPU at submit time. M1 has no background re-scheduler
		// (M2), so tell the user rather than hang forever.
		return fmt.Errorf("job %s queued: no free GPU matches right now (background scheduling lands in M2)", jobID)
	}

	// Assigned. Resolve the agent's address and upload the working directory.
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	agentAddr, err := nodeAddr(cfg.Coordinator, submitted.NodeID)
	if err != nil {
		return err
	}
	if err := uploadWorkdir(agentAddr, jobID, cwd); err != nil {
		return fmt.Errorf("uploading workdir to %s: %w", agentAddr, err)
	}
	fmt.Fprintf(os.Stderr, "flex: job %s assigned to %s (gpu %v)\n", jobID, submitted.NodeID, submitted.GPUIndexes)

	if *detach {
		fmt.Println(jobID)
		return nil
	}

	code, err := streamLogs(cfg.Coordinator, jobID)
	if err != nil {
		return err
	}
	os.Exit(code) // propagate the remote exit code
	return nil
}

// runLogs (re)attaches to a job's log stream and exits with its code.
func runLogs(args []string) error {
	fs := flag.NewFlagSet("logs", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() < 1 {
		return errors.New("usage: flex logs <job-id>")
	}
	cfg, err := mustConfig()
	if err != nil {
		return err
	}
	code, err := streamLogs(cfg.Coordinator, fs.Arg(0))
	if err != nil {
		return err
	}
	os.Exit(code)
	return nil
}

// runCancel requests cancellation of a job.
func runCancel(args []string) error {
	fs := flag.NewFlagSet("cancel", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() < 1 {
		return errors.New("usage: flex cancel <job-id>")
	}
	cfg, err := mustConfig()
	if err != nil {
		return err
	}
	if _, err := httpPostJSON(cfg.Coordinator+"/jobs/"+fs.Arg(0)+"/cancel", struct{}{}, nil); err != nil {
		return err
	}
	fmt.Printf("cancellation requested for %s\n", fs.Arg(0))
	return nil
}

// nodeAddr looks up a provider's advertised address via the GPU listing.
func nodeAddr(coordinator, nodeID string) (string, error) {
	var gpus []store.GPUStatus
	if err := httpGetJSON(coordinator+"/gpus", &gpus); err != nil {
		return "", err
	}
	for _, g := range gpus {
		if g.NodeID == nodeID && g.NodeAddr != "" {
			return strings.TrimRight(g.NodeAddr, "/"), nil
		}
	}
	return "", fmt.Errorf("could not resolve address of node %q", nodeID)
}

// uploadWorkdir streams a gzip tar of dir to the agent's workdir endpoint.
func uploadWorkdir(agentAddr, jobID, dir string) error {
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(workdir.Tar(dir, pw)) }()
	req, err := http.NewRequest(http.MethodPost, agentAddr+"/workdir/"+jobID, pr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return httpStatusErr(agentAddr+"/workdir/"+jobID, resp)
	}
	return nil
}

// streamLogs reads the SSE log stream, printing chunks to stdout/stderr, and
// returns the job's exit code when the "done" event arrives. It survives a
// dropped connection by reconnecting (the backlog ring replays missed output).
func streamLogs(coordinator, jobID string) (int, error) {
	url := coordinator + "/jobs/" + jobID + "/logs/stream"
	for attempt := 0; ; attempt++ {
		code, done, err := readSSEOnce(url)
		if done {
			return code, nil
		}
		if err == nil {
			// Stream ended without a done event (e.g. server closed); retry.
			err = errors.New("log stream closed before completion")
		}
		if attempt >= 5 {
			return 0, err
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// readSSEOnce consumes one SSE connection. Returns (exitCode, done=true) when a
// "done" event is seen; done=false means the connection ended early.
func readSSEOnce(url string) (int, bool, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, false, httpStatusErr(url, resp)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	var event, data string
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "": // event terminator
			if code, done := handleSSE(event, data); done {
				return code, true, nil
			}
			event, data = "", ""
		}
	}
	return 0, false, sc.Err()
}

func handleSSE(event, data string) (code int, done bool) {
	switch event {
	case "log":
		var ch types.LogChunk
		if json.Unmarshal([]byte(data), &ch) == nil {
			if ch.Stream == "stderr" {
				fmt.Fprint(os.Stderr, ch.Data)
			} else {
				fmt.Fprint(os.Stdout, ch.Data)
			}
		}
	case "done":
		var d struct {
			State    types.JobState `json:"state"`
			ExitCode int            `json:"exit_code"`
		}
		json.Unmarshal([]byte(data), &d)
		return d.ExitCode, true
	}
	return 0, false
}

func currentUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return firstNonEmpty(os.Getenv("USER"), "unknown")
}
