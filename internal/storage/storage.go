package storage

import (
	"database/sql"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type DB struct {
	db *sql.DB
}

type Check struct {
	EndpointID string
	Timestamp  time.Time
	StatusCode int
	LatencyMs  int64
	Up         bool
	Error      string
}

// HourBucket represents aggregated availability for a single hour slot.
type HourBucket struct {
	Hour      time.Time
	TotalUp   int
	TotalDown int
	AvgLatMs  float64
}

// Metric is a single sampled gauge value (queue depth, VRAM usage, ...).
type Metric struct {
	Name  string
	Label string // e.g. "cuda:0" for per-device metrics; "" otherwise
	Value float64
}

// MetricBucket is an hourly aggregate of one gauge metric.
type MetricBucket struct {
	Hour time.Time
	Avg  float64
	Min  float64
	Max  float64
}

// ComfyJob is one ComfyUI prompt execution. ComfyUI keeps its own history in
// memory only and loses it on restart, so this table is the durable record.
type ComfyJob struct {
	PromptID      string
	CreatedAt     int64 // unix ms; 0 = unknown
	StartedAt     int64 // unix ms; 0 = unknown
	EndedAt       int64 // unix ms; 0 = unknown
	Status        string
	Outputs       int
	NodesTotal    int
	NodesCached   int
	WorkflowID    string
	WaitEstimated bool
	Error         string
}

// DayStats aggregates ComfyUI jobs over one UTC calendar day.
type DayStats struct {
	Day           string  `json:"day"`
	Jobs          int     `json:"jobs"`
	Images        int     `json:"images"`
	OK            int     `json:"ok"`
	Failed        int     `json:"failed"`
	Cancelled     int     `json:"cancelled"`
	ExecP50Ms     int64   `json:"exec_p50_ms"`
	ExecP95Ms     int64   `json:"exec_p95_ms"`
	WaitP50Ms     int64   `json:"wait_p50_ms"`
	WaitP95Ms     int64   `json:"wait_p95_ms"`
	WaitEstimated bool    `json:"wait_estimated"`
	CacheHitRatio float64 `json:"cache_hit_ratio"`
}

func New(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir: %w", err)
	}
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &DB{db: db}, nil
}

func (d *DB) Close() error { return d.db.Close() }

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS checks (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			endpoint_id TEXT    NOT NULL,
			ts          INTEGER NOT NULL,
			status_code INTEGER NOT NULL DEFAULT 0,
			latency_ms  INTEGER NOT NULL DEFAULT 0,
			up          INTEGER NOT NULL DEFAULT 0,
			error       TEXT    NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_checks_endpoint_ts ON checks(endpoint_id, ts);

		CREATE TABLE IF NOT EXISTS metrics (
			source TEXT    NOT NULL,
			ts     INTEGER NOT NULL,
			name   TEXT    NOT NULL,
			label  TEXT    NOT NULL DEFAULT '',
			value  REAL    NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_metrics_lookup ON metrics(source, name, label, ts);

		CREATE TABLE IF NOT EXISTS comfy_jobs (
			prompt_id      TEXT PRIMARY KEY,
			created_at     INTEGER NOT NULL DEFAULT 0,
			started_at     INTEGER NOT NULL DEFAULT 0,
			ended_at       INTEGER NOT NULL DEFAULT 0,
			status         TEXT    NOT NULL DEFAULT '',
			outputs        INTEGER NOT NULL DEFAULT 0,
			nodes_total    INTEGER NOT NULL DEFAULT 0,
			nodes_cached   INTEGER NOT NULL DEFAULT 0,
			workflow_id    TEXT    NOT NULL DEFAULT '',
			wait_estimated INTEGER NOT NULL DEFAULT 0,
			error          TEXT    NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_comfy_jobs_ended ON comfy_jobs(ended_at);
	`)
	return err
}

func (d *DB) InsertCheck(c Check) error {
	_, err := d.db.Exec(
		`INSERT INTO checks(endpoint_id, ts, status_code, latency_ms, up, error) VALUES (?,?,?,?,?,?)`,
		c.EndpointID, c.Timestamp.Unix(), c.StatusCode, c.LatencyMs,
		boolToInt(c.Up), c.Error,
	)
	return err
}

// GetHourBuckets returns hourly aggregated data for an endpoint over the last N days.
func (d *DB) GetHourBuckets(endpointID string, days int) ([]HourBucket, error) {
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()

	rows, err := d.db.Query(`
		SELECT
			(ts / 3600) * 3600 AS hour_ts,
			SUM(up)            AS total_up,
			COUNT(*) - SUM(up) AS total_down,
			AVG(latency_ms)    AS avg_lat
		FROM checks
		WHERE endpoint_id = ? AND ts >= ?
		GROUP BY hour_ts
		ORDER BY hour_ts ASC
	`, endpointID, since)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var buckets []HourBucket
	for rows.Next() {
		var b HourBucket
		var hourTs int64
		if err := rows.Scan(&hourTs, &b.TotalUp, &b.TotalDown, &b.AvgLatMs); err != nil {
			return nil, err
		}
		b.Hour = time.Unix(hourTs, 0).UTC()
		buckets = append(buckets, b)
	}
	return buckets, rows.Err()
}

// GetLatestCheck returns the most recent check result for an endpoint.
func (d *DB) GetLatestCheck(endpointID string) (*Check, error) {
	row := d.db.QueryRow(`
		SELECT endpoint_id, ts, status_code, latency_ms, up, error
		FROM checks WHERE endpoint_id = ?
		ORDER BY ts DESC LIMIT 1
	`, endpointID)

	var c Check
	var ts int64
	var up int
	err := row.Scan(&c.EndpointID, &ts, &c.StatusCode, &c.LatencyMs, &up, &c.Error)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Timestamp = time.Unix(ts, 0).UTC()
	c.Up = up == 1
	return &c, nil
}

// InsertMetrics writes one sample (several named gauges sharing a timestamp).
func (d *DB) InsertMetrics(source string, ts time.Time, ms []Metric) error {
	if len(ms) == 0 {
		return nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.Prepare(`INSERT INTO metrics(source, ts, name, label, value) VALUES (?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	unix := ts.Unix()
	for _, m := range ms {
		if _, err := stmt.Exec(source, unix, m.Name, m.Label, m.Value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// GetMetricBuckets returns hourly avg/min/max of one gauge over the last N days.
func (d *DB) GetMetricBuckets(source, name, label string, days int) ([]MetricBucket, error) {
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()

	rows, err := d.db.Query(`
		SELECT
			(ts / 3600) * 3600 AS hour_ts,
			AVG(value) AS avg_v,
			MIN(value) AS min_v,
			MAX(value) AS max_v
		FROM metrics
		WHERE source = ? AND name = ? AND label = ? AND ts >= ?
		GROUP BY hour_ts
		ORDER BY hour_ts ASC
	`, source, name, label, since)
	if err != nil {
		return nil, fmt.Errorf("query metrics: %w", err)
	}
	defer rows.Close()

	var out []MetricBucket
	for rows.Next() {
		var b MetricBucket
		var hourTs int64
		if err := rows.Scan(&hourTs, &b.Avg, &b.Min, &b.Max); err != nil {
			return nil, err
		}
		b.Hour = time.Unix(hourTs, 0).UTC()
		out = append(out, b)
	}
	return out, rows.Err()
}

// MetricLabels lists the distinct labels seen for a metric within the window.
// Used to discover how many GPUs reported without hardcoding a device count.
func (d *DB) MetricLabels(source, name string, days int) ([]string, error) {
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).Unix()
	rows, err := d.db.Query(`
		SELECT DISTINCT label FROM metrics
		WHERE source = ? AND name = ? AND ts >= ?
		ORDER BY label ASC
	`, source, name, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// UpsertComfyJob inserts or updates a job by prompt_id. Re-reading the same
// ComfyUI history window must never double-count, so this is the only write path.
func (d *DB) UpsertComfyJob(j ComfyJob) error {
	_, err := d.db.Exec(`
		INSERT INTO comfy_jobs
			(prompt_id, created_at, started_at, ended_at, status, outputs,
			 nodes_total, nodes_cached, workflow_id, wait_estimated, error)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(prompt_id) DO UPDATE SET
			created_at     = CASE WHEN excluded.created_at > 0 THEN excluded.created_at ELSE comfy_jobs.created_at END,
			started_at     = CASE WHEN excluded.started_at > 0 THEN excluded.started_at ELSE comfy_jobs.started_at END,
			ended_at       = CASE WHEN excluded.ended_at   > 0 THEN excluded.ended_at   ELSE comfy_jobs.ended_at   END,
			status         = excluded.status,
			outputs        = excluded.outputs,
			nodes_total    = excluded.nodes_total,
			nodes_cached   = excluded.nodes_cached,
			workflow_id    = excluded.workflow_id,
			wait_estimated = excluded.wait_estimated,
			error          = excluded.error
	`,
		j.PromptID, j.CreatedAt, j.StartedAt, j.EndedAt, j.Status, j.Outputs,
		j.NodesTotal, j.NodesCached, j.WorkflowID, boolToInt(j.WaitEstimated), j.Error,
	)
	return err
}

// HasComfyJob reports whether a prompt_id is already recorded. Used by the
// poller to detect that a history page was entirely new (i.e. we may have
// missed jobs between drains).
func (d *DB) HasComfyJob(promptID string) (bool, error) {
	var one int
	err := d.db.QueryRow(`SELECT 1 FROM comfy_jobs WHERE prompt_id = ?`, promptID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// LastComfyJobEnd returns the newest ended_at across all recorded jobs (unix ms, 0 if none).
func (d *DB) LastComfyJobEnd() (int64, error) {
	var v sql.NullInt64
	if err := d.db.QueryRow(`SELECT MAX(ended_at) FROM comfy_jobs`).Scan(&v); err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// GetComfyStats aggregates jobs over the last N days, both as one overall
// summary and split per UTC day. Percentiles are computed in Go — SQLite has no
// percentile function without extensions, and the job count over 30 days is
// small enough to sort in memory.
func (d *DB) GetComfyStats(days int) (DayStats, []DayStats, error) {
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli()

	rows, err := d.db.Query(`
		SELECT created_at, started_at, ended_at, status, outputs,
		       nodes_total, nodes_cached, wait_estimated
		FROM comfy_jobs
		WHERE ended_at >= ?
		ORDER BY ended_at ASC
	`, since)
	if err != nil {
		return DayStats{}, nil, fmt.Errorf("query comfy jobs: %w", err)
	}
	defer rows.Close()

	type acc struct {
		st          DayStats
		execs       []int64
		waits       []int64
		nodesTotal  int
		nodesCached int
	}
	byDay := map[string]*acc{}
	var order []string
	all := &acc{}

	add := func(a *acc, createdAt, startedAt, endedAt int64, status string,
		outputs, nodesTotal, nodesCached, waitEst int) {
		a.st.Jobs++
		a.st.Images += outputs
		switch status {
		case "success":
			a.st.OK++
		case "cancelled", "interrupted":
			a.st.Cancelled++
		default:
			a.st.Failed++
		}
		if startedAt > 0 && endedAt > startedAt {
			a.execs = append(a.execs, endedAt-startedAt)
		}
		// A job that started the instant it was queued has a legitimate wait of
		// zero, so record that rather than dropping it from the percentile.
		if createdAt > 0 && startedAt > 0 {
			wait := startedAt - createdAt
			if wait < 0 {
				wait = 0
			}
			a.waits = append(a.waits, wait)
		}
		if waitEst == 1 {
			a.st.WaitEstimated = true
		}
		a.nodesTotal += nodesTotal
		a.nodesCached += nodesCached
	}

	for rows.Next() {
		var createdAt, startedAt, endedAt int64
		var status string
		var outputs, nodesTotal, nodesCached, waitEst int
		if err := rows.Scan(&createdAt, &startedAt, &endedAt, &status, &outputs,
			&nodesTotal, &nodesCached, &waitEst); err != nil {
			return DayStats{}, nil, err
		}

		day := time.UnixMilli(endedAt).UTC().Format("2006-01-02")
		a := byDay[day]
		if a == nil {
			a = &acc{st: DayStats{Day: day}}
			byDay[day] = a
			order = append(order, day)
		}
		add(a, createdAt, startedAt, endedAt, status, outputs, nodesTotal, nodesCached, waitEst)
		add(all, createdAt, startedAt, endedAt, status, outputs, nodesTotal, nodesCached, waitEst)
	}
	if err := rows.Err(); err != nil {
		return DayStats{}, nil, err
	}

	finish := func(a *acc) DayStats {
		a.st.ExecP50Ms = percentile(a.execs, 0.50)
		a.st.ExecP95Ms = percentile(a.execs, 0.95)
		a.st.WaitP50Ms = percentile(a.waits, 0.50)
		a.st.WaitP95Ms = percentile(a.waits, 0.95)
		if a.nodesTotal > 0 {
			a.st.CacheHitRatio = math.Round(float64(a.nodesCached)/float64(a.nodesTotal)*1000) / 1000
		}
		return a.st
	}

	out := make([]DayStats, 0, len(order))
	for _, day := range order {
		out = append(out, finish(byDay[day]))
	}
	return finish(all), out, nil
}

// percentile returns the p-th percentile (nearest-rank) of vs, or 0 if empty.
func percentile(vs []int64, p float64) int64 {
	if len(vs) == 0 {
		return 0
	}
	s := make([]int64, len(vs))
	copy(s, vs)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(math.Ceil(p*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

// PruneOld deletes records older than retainDays.
func (d *DB) PruneOld(retainDays int) error {
	cutoff := time.Now().Add(-time.Duration(retainDays) * 24 * time.Hour)
	if _, err := d.db.Exec(`DELETE FROM checks WHERE ts < ?`, cutoff.Unix()); err != nil {
		return fmt.Errorf("prune checks: %w", err)
	}
	if _, err := d.db.Exec(`DELETE FROM metrics WHERE ts < ?`, cutoff.Unix()); err != nil {
		return fmt.Errorf("prune metrics: %w", err)
	}
	if _, err := d.db.Exec(`DELETE FROM comfy_jobs WHERE ended_at > 0 AND ended_at < ?`, cutoff.UnixMilli()); err != nil {
		return fmt.Errorf("prune comfy_jobs: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
