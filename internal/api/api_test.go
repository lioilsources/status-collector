package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ol1n/status-collector/internal/checker"
	"github.com/ol1n/status-collector/internal/storage"
)

func newTestServer(t *testing.T) (*Server, *storage.DB) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	eps := []checker.Endpoint{
		{ID: "health", Name: "vLLM Health", Group: "Health", URL: "https://example.test/health"},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// nil poller — ComfyUI monitoring disabled
	return New(db, logger, eps, nil, nil), db
}

func TestStatusEndpoint(t *testing.T) {
	srv, db := newTestServer(t)

	now := time.Now().UTC()
	for i, up := range []bool{true, true, false} {
		err := db.InsertCheck(storage.Check{
			EndpointID: "health",
			Timestamp:  now.Add(-time.Duration(i) * time.Hour),
			StatusCode: 200,
			LatencyMs:  int64(100 + i),
			Up:         up,
		})
		if err != nil {
			t.Fatalf("InsertCheck: %v", err)
		}
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	// The frontend now lives on a different origin, so this header is load-bearing.
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS header = %q; want *", got)
	}

	var resp StatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Endpoints) != 1 {
		t.Fatalf("endpoints = %d; want 1", len(resp.Endpoints))
	}
	ep := resp.Endpoints[0]
	if ep.Current == nil || !ep.Current.Up {
		t.Errorf("current = %+v; want the most recent check, which is up", ep.Current)
	}
	if len(ep.Buckets) != 3 {
		t.Errorf("buckets = %d; want 3", len(ep.Buckets))
	}
	want := 2.0 / 3.0 * 100
	if diff := ep.Uptime30 - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("uptime_30d = %v; want ~%v", ep.Uptime30, want)
	}
}

func TestHistoryEndpointUsesSnakeCase(t *testing.T) {
	srv, db := newTestServer(t)
	if err := db.InsertCheck(storage.Check{
		EndpointID: "health", Timestamp: time.Now().UTC(), StatusCode: 200, Up: true,
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/history/health", nil))

	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("entries = %d; want 1", len(out))
	}
	for _, key := range []string{"hour", "up", "down", "avail", "avg_lat_ms"} {
		if _, ok := out[0][key]; !ok {
			t.Errorf("missing key %q; got %v", key, out[0])
		}
	}
}

func TestComfyEndpointDisabled(t *testing.T) {
	srv, _ := newTestServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/comfy", nil))

	var resp ComfyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Enabled {
		t.Error("enabled should be false without a poller")
	}
	// Empty slices, not nulls — the frontend iterates these unconditionally.
	for _, raw := range []string{`"queue_depth":[]`, `"vram_used_pct":[]`, `"days":[]`} {
		if !strings.Contains(rec.Body.String(), raw) {
			t.Errorf("payload should contain %s; got %s", raw, rec.Body.String())
		}
	}
}

func TestWriteSnapshot(t *testing.T) {
	srv, db := newTestServer(t)
	if err := db.InsertCheck(storage.Check{
		EndpointID: "health", Timestamp: time.Now().UTC(), StatusCode: 200, Up: true,
	}); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "snap")
	if err := srv.WriteSnapshot(dir); err != nil {
		t.Fatalf("WriteSnapshot: %v", err)
	}

	for _, name := range []string{"status.json", "comfy.json", "hosts.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Errorf("%s is not valid JSON: %v", name, err)
		}
		if _, ok := v["generated_at"]; !ok {
			t.Errorf("%s has no generated_at — the frontend needs it for the staleness chip", name)
		}
	}

	// No temp files left behind: the publish script copies the whole directory.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("snapshot dir has %d entries; want status.json, comfy.json and hosts.json", len(entries))
	}

	// Rewriting must overwrite in place, not fail on an existing file.
	if err := srv.WriteSnapshot(dir); err != nil {
		t.Fatalf("second WriteSnapshot: %v", err)
	}
}
