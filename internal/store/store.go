// Package store is the coordinator's persistent state: nodes, their GPUs, and
// jobs, backed by SQLite (pure-Go modernc driver, so the coordinator stays a
// single cgo-free static binary that goreleaser can cross-compile).
//
// The assignment ledger lives in gpus.assigned_job — this is the SINGLE SOURCE
// OF TRUTH for "is this GPU free". nvidia-smi utilization is never consulted
// for scheduling. Assign reserves GPUs inside a BEGIN IMMEDIATE transaction so
// two clients racing for the last GPU cannot both win:
//
//	client A ─┐                  ┌─ BEGIN IMMEDIATE (gets write lock)
//	          ├─ Assign(job) ────┤   reserve gpu, COMMIT            ── wins
//	client B ─┘                  └─ BEGIN IMMEDIATE (waits busy_timeout)
//	                                 sees 0 free, ROLLBACK          ── ErrNoFreeGPU
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/PaulOh5/gpu-private-cloud-with-gstack/internal/types"
)

// ErrNoFreeGPU is returned by Assign when no free GPU satisfies the job's spec
// (wrong model, not enough VRAM, or all GPUs busy). The caller leaves the job
// queued and retries later.
var ErrNoFreeGPU = errors.New("no free GPU matches the job spec")

// ErrJobNotFound is returned when a job id is unknown.
var ErrJobNotFound = errors.New("job not found")

// Store wraps the SQLite database. Safe for concurrent use: SQLite serializes
// writers and the driver waits up to busy_timeout instead of erroring.
type Store struct {
	db *sql.DB
}

// GPUStatus is a GPU plus its scheduling state, for `flex gpus`.
type GPUStatus struct {
	types.GPU
	NodeAddr    string `json:"node_addr"`
	AssignedJob string `json:"assigned_job"` // "" means free
	Free        bool   `json:"free"`
}

// Open opens (creating if needed) the SQLite database at path and applies the
// schema. WAL mode lets `flex gpus` reads run concurrently with assign writes;
// busy_timeout makes contending writers wait rather than fail immediately.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS nodes (
  id             TEXT PRIMARY KEY,
  addr           TEXT NOT NULL,
  role           TEXT NOT NULL,
  last_heartbeat INTEGER NOT NULL  -- unix seconds
);
CREATE TABLE IF NOT EXISTS gpus (
  node_id      TEXT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
  idx          INTEGER NOT NULL,
  name         TEXT NOT NULL,
  mem_total_mb INTEGER NOT NULL,
  mem_used_mb  INTEGER NOT NULL,
  util_percent INTEGER NOT NULL,
  assigned_job TEXT NOT NULL DEFAULT '',  -- ledger: '' = free
  PRIMARY KEY (node_id, idx)
);
CREATE TABLE IF NOT EXISTS jobs (
  id          TEXT PRIMARY KEY,
  name        TEXT NOT NULL,
  user        TEXT NOT NULL,
  command     TEXT NOT NULL,  -- JSON []string
  env_setup   TEXT NOT NULL,
  spec        TEXT NOT NULL,  -- JSON types.GPUSpec
  vram_min_mb INTEGER NOT NULL,
  state       TEXT NOT NULL,
  node_id     TEXT NOT NULL DEFAULT '',
  gpu_indexes TEXT NOT NULL DEFAULT '[]',  -- JSON []int
  exit_code   INTEGER NOT NULL DEFAULT 0,
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);`
	if _, err := s.db.Exec(schema); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// RegisterNode upserts a node and its GPUs. Existing GPUs are updated in place
// WITHOUT clearing assigned_job, so a re-register (agent restart) does not free
// a GPU that is still running a job — the M1 guard against double-assignment.
// GPUs no longer reported are removed.
func (s *Store) RegisterNode(ctx context.Context, n types.Node) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes (id, addr, role, last_heartbeat) VALUES (?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET addr=excluded.addr, role=excluded.role, last_heartbeat=excluded.last_heartbeat`,
		n.ID, n.Addr, string(n.Role), n.LastHeartbeat.Unix(),
	); err != nil {
		return fmt.Errorf("upsert node: %w", err)
	}

	keep := make(map[int]bool, len(n.GPUs))
	for _, g := range n.GPUs {
		keep[g.Index] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO gpus (node_id, idx, name, mem_total_mb, mem_used_mb, util_percent)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(node_id, idx) DO UPDATE SET
			   name=excluded.name, mem_total_mb=excluded.mem_total_mb,
			   mem_used_mb=excluded.mem_used_mb, util_percent=excluded.util_percent`,
			n.ID, g.Index, g.Name, g.MemTotalMB, g.MemUsedMB, g.UtilPercent,
		); err != nil {
			return fmt.Errorf("upsert gpu %d: %w", g.Index, err)
		}
	}
	// Remove GPUs the node no longer reports (e.g. card pulled).
	rows, err := tx.QueryContext(ctx, `SELECT idx FROM gpus WHERE node_id = ?`, n.ID)
	if err != nil {
		return err
	}
	var stale []int
	for rows.Next() {
		var idx int
		if err := rows.Scan(&idx); err != nil {
			rows.Close()
			return err
		}
		if !keep[idx] {
			stale = append(stale, idx)
		}
	}
	rows.Close()
	for _, idx := range stale {
		if _, err := tx.ExecContext(ctx, `DELETE FROM gpus WHERE node_id = ? AND idx = ?`, n.ID, idx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Heartbeat updates a node's liveness timestamp.
func (s *Store) Heartbeat(ctx context.Context, nodeID string, t time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE nodes SET last_heartbeat = ? WHERE id = ?`, t.Unix(), nodeID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("heartbeat: unknown node %q", nodeID)
	}
	return nil
}

// ListGPUs returns every GPU in the pool with its free/busy state, for `flex gpus`.
func (s *Store) ListGPUs(ctx context.Context) ([]GPUStatus, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT g.node_id, g.idx, g.name, g.mem_total_mb, g.mem_used_mb, g.util_percent, g.assigned_job, n.addr
		 FROM gpus g JOIN nodes n ON n.id = g.node_id
		 ORDER BY g.node_id, g.idx`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GPUStatus
	for rows.Next() {
		var gs GPUStatus
		if err := rows.Scan(&gs.NodeID, &gs.Index, &gs.Name, &gs.MemTotalMB,
			&gs.MemUsedMB, &gs.UtilPercent, &gs.AssignedJob, &gs.NodeAddr); err != nil {
			return nil, err
		}
		gs.Free = gs.AssignedJob == ""
		out = append(out, gs)
	}
	return out, rows.Err()
}

// SubmitJob inserts a new job in the queued state.
func (s *Store) SubmitJob(ctx context.Context, j types.Job) error {
	cmd, _ := json.Marshal(j.Command)
	spec, _ := json.Marshal(j.Spec)
	idxs, _ := json.Marshal(j.GPUIndexes)
	if len(j.GPUIndexes) == 0 {
		idxs = []byte("[]")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (id, name, user, command, env_setup, spec, vram_min_mb, state, node_id, gpu_indexes, exit_code, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, 0, ?, ?)`,
		j.ID, j.Name, j.User, string(cmd), j.EnvSetup, string(spec), j.VRAMMinMB,
		string(types.JobQueued), string(idxs), j.CreatedAt.Unix(), j.UpdatedAt.Unix(),
	)
	return err
}

// GetJob loads a job by id.
func (s *Store) GetJob(ctx context.Context, id string) (types.Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, jobCols+` FROM jobs WHERE id = ?`, id))
}

// Assign reserves GPUs for a queued job inside a BEGIN IMMEDIATE transaction.
// It assigns spec.Count GPUs that all live on a single provider with a recent
// heartbeat, match the model, and meet the VRAM minimum. On success the job
// moves to assigned with node_id + gpu_indexes set. If nothing fits, the job
// stays queued and ErrNoFreeGPU is returned.
//
// liveWithin bounds provider liveness: GPUs on a node whose last heartbeat is
// older than now-liveWithin are not assignable.
func (s *Store) Assign(ctx context.Context, jobID string, now time.Time, liveWithin time.Duration) (types.Job, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return types.Job{}, err
	}
	defer conn.Close()

	// Explicit BEGIN IMMEDIATE: take the write lock up front so two concurrent
	// assigns serialize cleanly (a deferred BEGIN would read-lock first and can
	// deadlock on lock upgrade).
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return types.Job{}, fmt.Errorf("begin immediate: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	job, err := scanJob(conn.QueryRowContext(ctx, jobCols+` FROM jobs WHERE id = ?`, jobID))
	if err != nil {
		return types.Job{}, err
	}
	if job.State != types.JobQueued {
		return types.Job{}, fmt.Errorf("job %s is %s, not queued", jobID, job.State)
	}

	minStamp := now.Add(-liveWithin).Unix()
	rows, err := conn.QueryContext(ctx,
		`SELECT g.node_id, g.idx, g.name, g.mem_total_mb
		   FROM gpus g JOIN nodes n ON n.id = g.node_id
		  WHERE g.assigned_job = '' AND n.last_heartbeat >= ?
		  ORDER BY g.node_id, g.idx`, minStamp)
	if err != nil {
		return types.Job{}, err
	}
	// Group free GPUs by node so a multi-GPU job lands on one provider.
	type cand struct {
		idx  int
		name string
		vram int
	}
	byNode := map[string][]cand{}
	order := []string{}
	for rows.Next() {
		var nodeID, name string
		var idx, vram int
		if err := rows.Scan(&nodeID, &idx, &name, &vram); err != nil {
			rows.Close()
			return types.Job{}, err
		}
		if _, ok := byNode[nodeID]; !ok {
			order = append(order, nodeID)
		}
		byNode[nodeID] = append(byNode[nodeID], cand{idx, name, vram})
	}
	rows.Close()

	count := job.Spec.Count
	if count < 1 {
		count = 1
	}
	var pickNode string
	var pickIdx []int
	for _, nodeID := range order {
		var matched []int
		for _, c := range byNode[nodeID] {
			if !job.Spec.Matches(types.GPU{Name: c.name}) {
				continue
			}
			if job.VRAMMinMB > 0 && c.vram < job.VRAMMinMB {
				continue
			}
			matched = append(matched, c.idx)
			if len(matched) == count {
				break
			}
		}
		if len(matched) == count {
			pickNode, pickIdx = nodeID, matched
			break
		}
	}
	if pickNode == "" {
		return types.Job{}, ErrNoFreeGPU
	}

	for _, idx := range pickIdx {
		if _, err := conn.ExecContext(ctx,
			`UPDATE gpus SET assigned_job = ? WHERE node_id = ? AND idx = ?`, jobID, pickNode, idx); err != nil {
			return types.Job{}, err
		}
	}
	idxJSON, _ := json.Marshal(pickIdx)
	if _, err := conn.ExecContext(ctx,
		`UPDATE jobs SET state = ?, node_id = ?, gpu_indexes = ?, updated_at = ? WHERE id = ?`,
		string(types.JobAssigned), pickNode, string(idxJSON), now.Unix(), jobID); err != nil {
		return types.Job{}, err
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return types.Job{}, fmt.Errorf("commit: %w", err)
	}
	committed = true

	job.State = types.JobAssigned
	job.NodeID = pickNode
	job.GPUIndexes = pickIdx
	job.UpdatedAt = now
	return job, nil
}

// jobCols is the column list scanJob expects, kept beside the scanner.
const jobCols = `SELECT id, name, user, command, env_setup, spec, vram_min_mb, state, node_id, gpu_indexes, exit_code, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (types.Job, error) {
	var j types.Job
	var cmd, spec, idxs string
	var created, updated int64
	err := row.Scan(&j.ID, &j.Name, &j.User, &cmd, &j.EnvSetup, &spec, &j.VRAMMinMB,
		&j.State, &j.NodeID, &idxs, &j.ExitCode, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return types.Job{}, ErrJobNotFound
	}
	if err != nil {
		return types.Job{}, err
	}
	json.Unmarshal([]byte(cmd), &j.Command)
	json.Unmarshal([]byte(spec), &j.Spec)
	json.Unmarshal([]byte(idxs), &j.GPUIndexes)
	j.CreatedAt = time.Unix(created, 0)
	j.UpdatedAt = time.Unix(updated, 0)
	return j, nil
}
