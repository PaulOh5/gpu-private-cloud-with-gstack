// Command flex is the single binary for the GPU pool: coordinator, provider
// agent, and client CLI, selected by subcommand.
//
//	flex coordinator   # run the central coordinator (control plane)
//	flex join <url>    # point this machine at a coordinator
//	flex agent         # contribute this machine's GPUs (provider)
//	flex gpus          # list the pool's GPUs (client)
//	flex run -- cmd    # run a command on a free GPU (client)
//	flex logs <job>    # (re)attach to a job's logs (client)
//	flex cancel <job>  # cancel a job (client)
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "coordinator":
		err = runCoordinator(args)
	case "join":
		err = runJoin(args)
	case "agent":
		err = runAgent(args)
	case "gpus":
		err = runGPUs(args)
	case "run":
		err = runRun(args) // may call os.Exit with the remote exit code
	case "logs":
		err = runLogs(args)
	case "cancel":
		err = runCancel(args)
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "flex: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "flex: "+err.Error())
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `flex — a private GPU pool

Usage:
  flex coordinator [--db PATH] [--addr :7070]
  flex join <coordinator-url> [--name NODE]
  flex agent [--name NODE] [--listen :7071] [--advertise URL] [--run-as USER]
  flex gpus
  flex run [--gpu any|MODEL|count:N] [--vram GB] [--name NAME] [--env SETUP] [--detach] -- CMD...
  flex logs <job-id>
  flex cancel <job-id>
`)
}

// newID returns a short random job/run id.
func newID() string {
	var b [6]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// httpGetJSON GETs url and decodes the JSON body into out.
func httpGetJSON(url string, out any) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return httpStatusErr(url, resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// httpPostJSON POSTs in as JSON and, if out != nil, decodes the response.
// Returns the HTTP status so callers can distinguish 201 (assigned) vs 202
// (queued). A non-2xx status is an error.
func httpPostJSON(url string, in, out any) (int, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return 0, err
		}
		body = strings.NewReader(string(b))
	}
	resp, err := http.Post(url, "application/json", body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, httpStatusErr(url, resp)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

func httpStatusErr(url string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
	return fmt.Errorf("%s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(b)))
}
