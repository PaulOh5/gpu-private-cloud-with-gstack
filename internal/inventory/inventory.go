// Package inventory detects the local NVIDIA GPUs a provider contributes to
// the pool by shelling out to `nvidia-smi` and parsing its CSV output.
//
// Parsing is split from execution on purpose: Parse is a pure function over
// raw bytes (table-driven testable, no hardware needed — this is also how the
// mock-GPU E2E feeds fake inventory), while Detect handles the side effect of
// locating and running nvidia-smi.
//
//	Detect(ctx,nodeID)
//	   │  exec.LookPath("nvidia-smi")  ──not found──▶ ErrNoNvidiaSmi
//	   │  run --query-gpu=... --format=csv,noheader,nounits
//	   ▼
//	Parse(nodeID, raw) ──▶ []types.GPU   (empty slice = 0 GPUs, NOT an error)
package inventory

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// ErrNoNvidiaSmi means nvidia-smi is not installed / not on PATH. Callers
// (the agent) treat this as "this machine has no GPUs to contribute" and run
// as a pure client rather than failing — a GPU-less laptop is a valid node.
var ErrNoNvidiaSmi = errors.New("nvidia-smi not found on PATH")

// queryFields is the exact column order Parse expects. Kept next to the parser
// so the query and the parser never drift apart.
const queryFields = "index,name,memory.total,memory.used,utilization.gpu"

// Detector runs nvidia-smi and parses its output. The run hook is injectable so
// tests (and the mock-GPU harness) can supply canned output without hardware.
type Detector struct {
	// run returns the raw CSV bytes of an nvidia-smi query. Defaults to the
	// real binary when zero-valued (see Detect).
	run func(ctx context.Context) ([]byte, error)
}

// Detect returns the GPUs on this machine, tagged with nodeID.
//
// It distinguishes three outcomes the agent cares about:
//   - nvidia-smi missing      → ErrNoNvidiaSmi (run as client, not a failure)
//   - nvidia-smi ran, 0 GPUs  → empty slice, nil error
//   - malformed output        → parse error (real problem, surfaced clearly)
func (d Detector) Detect(ctx context.Context, nodeID string) ([]types.GPU, error) {
	run := d.run
	if run == nil {
		run = runNvidiaSmi
	}
	raw, err := run(ctx)
	if err != nil {
		return nil, err
	}
	return Parse(nodeID, raw)
}

// runNvidiaSmi is the default Detector.run: locate and execute nvidia-smi.
func runNvidiaSmi(ctx context.Context) ([]byte, error) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		return nil, ErrNoNvidiaSmi
	}
	cmd := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu="+queryFields,
		"--format=csv,noheader,nounits",
	)
	out, err := cmd.Output()
	if err != nil {
		// A machine with the driver but no GPU prints "No devices were found"
		// and exits non-zero; Parse handles that line as 0 GPUs, so feed
		// stdout through rather than treating the exit code as fatal.
		if ee := (*exec.ExitError)(nil); errors.As(err, &ee) {
			return out, nil
		}
		return nil, fmt.Errorf("running nvidia-smi: %w", err)
	}
	return out, nil
}

// Parse turns nvidia-smi CSV (--format=csv,noheader,nounits) into GPUs.
//
// Expected line: "<index>, <name>, <mem_total>, <mem_used>, <util>"
// Empty input or a "No devices were found" line yields zero GPUs (nil error).
// Any other malformed line is an error naming the offending line — an explicit
// failure beats silently dropping a GPU the scheduler would then never see.
func Parse(nodeID string, raw []byte) ([]types.GPU, error) {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return []types.GPU{}, nil
	}

	var gpus []types.GPU
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(strings.ToLower(line), "no devices were found") {
			return []types.GPU{}, nil
		}

		fields := strings.Split(line, ",")
		if len(fields) != 5 {
			return nil, fmt.Errorf("line %d: expected 5 csv fields (%s), got %d: %q",
				i+1, queryFields, len(fields), line)
		}
		for j := range fields {
			fields[j] = strings.TrimSpace(fields[j])
		}

		index, err := atoiField(i+1, "index", fields[0])
		if err != nil {
			return nil, err
		}
		memTotal, err := atoiField(i+1, "memory.total", fields[2])
		if err != nil {
			return nil, err
		}
		memUsed, err := atoiField(i+1, "memory.used", fields[3])
		if err != nil {
			return nil, err
		}
		util, err := atoiField(i+1, "utilization.gpu", fields[4])
		if err != nil {
			return nil, err
		}

		gpus = append(gpus, types.GPU{
			NodeID:      nodeID,
			Index:       index,
			Name:        fields[1],
			MemTotalMB:  memTotal,
			MemUsedMB:   memUsed,
			UtilPercent: util,
		})
	}
	return gpus, nil
}

// atoiField parses one integer column, wrapping the error with enough context
// (line number + column name + raw value) to debug a format mismatch at 3am.
func atoiField(lineNo int, col, val string) (int, error) {
	// nvidia-smi can emit "[N/A]" for an unsupported field; treat as 0 so a
	// driver quirk on one column doesn't drop the whole GPU.
	if strings.EqualFold(val, "[N/A]") || val == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("line %d: %s = %q is not an integer", lineNo, col, val)
	}
	return n, nil
}
