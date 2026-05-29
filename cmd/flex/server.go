package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/agent"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/config"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/coordinator"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/inventory"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/store"
	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// runCoordinator starts the central control plane.
func runCoordinator(args []string) error {
	fs := flag.NewFlagSet("coordinator", flag.ExitOnError)
	addr := fs.String("addr", ":7070", "listen address")
	dbPath := fs.String("db", defaultDBPath(), "SQLite database path")
	fs.Parse(args)

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		return err
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	_, handler := coordinator.New(st)

	srv := &http.Server{Addr: *addr, Handler: handler}
	go shutdownOnSignal(srv)
	fmt.Printf("flex coordinator listening on %s (db: %s)\n", *addr, *dbPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// runAgent contributes this machine's GPUs and executes assigned jobs.
func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	name := fs.String("name", "", "node id (default: config node or hostname)")
	listen := fs.String("listen", ":7071", "address to receive workdir uploads on")
	advertise := fs.String("advertise", "", "URL clients use to reach this agent (default: http://<hostname><listen>)")
	runAs := fs.String("run-as", "", "unprivileged user to run jobs as (recommended on shared providers)")
	fs.Parse(args)

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Coordinator == "" {
		return errors.New("no coordinator configured; run `flex join <coordinator-url>` first")
	}
	nodeID := firstNonEmpty(*name, cfg.Node, hostname())
	adv := *advertise
	if adv == "" {
		adv = "http://" + hostname() + *listen
	}

	// FLEX_MOCK_GPUS (comma-separated model names) fakes inventory so the agent
	// runs without NVIDIA hardware — used by the localhost mock-GPU E2E and for
	// kicking the tires locally. Real providers leave it unset.
	gpus, err := mockOrDetectGPUs(nodeID)
	if err != nil && !errors.Is(err, inventory.ErrNoNvidiaSmi) {
		return fmt.Errorf("detecting GPUs: %w", err)
	}
	if errors.Is(err, inventory.ErrNoNvidiaSmi) || len(gpus) == 0 {
		fmt.Println("flex agent: no NVIDIA GPUs detected — this node will contribute none")
	} else {
		fmt.Printf("flex agent: contributing %d GPU(s)\n", len(gpus))
	}

	stager, err := agent.NewWorkdirStager(filepath.Join(os.TempDir(), "flex-workdirs"))
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Data plane: receive client workdir uploads.
	wsrv := &http.Server{Addr: *listen, Handler: stager.Handler()}
	go func() {
		if err := wsrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "flex agent: workdir server: "+err.Error())
		}
	}()
	defer func() {
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		wsrv.Shutdown(sctx)
	}()

	a := &agent.Agent{
		NodeID:    nodeID,
		Addr:      adv,
		Control:   agent.NewHTTPControl(cfg.Coordinator),
		RunAsUser: *runAs,
		Stager:    stager,
	}
	fmt.Printf("flex agent %q → coordinator %s (workdir uploads at %s)\n", nodeID, cfg.Coordinator, adv)
	if err := a.Run(ctx, gpus); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

func shutdownOnSignal(srv *http.Server) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
}

// mockOrDetectGPUs returns synthetic GPUs from FLEX_MOCK_GPUS when set,
// otherwise the real nvidia-smi inventory.
func mockOrDetectGPUs(nodeID string) ([]types.GPU, error) {
	if spec := os.Getenv("FLEX_MOCK_GPUS"); spec != "" {
		var gpus []types.GPU
		for i, name := range strings.Split(spec, ",") {
			gpus = append(gpus, types.GPU{NodeID: nodeID, Index: i, Name: strings.TrimSpace(name), MemTotalMB: 24000})
		}
		return gpus, nil
	}
	return inventory.Detector{}.Detect(context.Background(), nodeID)
}

func defaultDBPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "flex-coordinator.db"
	}
	return filepath.Join(home, ".flex", "coordinator.db")
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "flex-node"
	}
	return h
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
