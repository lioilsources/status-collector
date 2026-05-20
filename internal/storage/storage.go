package storage

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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

// PruneOld deletes records older than retainDays.
func (d *DB) PruneOld(retainDays int) error {
	cutoff := time.Now().Add(-time.Duration(retainDays) * 24 * time.Hour).Unix()
	_, err := d.db.Exec(`DELETE FROM checks WHERE ts < ?`, cutoff)
	return err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
