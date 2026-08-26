package comfy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ol1n/status-collector/internal/storage"
)

// Fixture timestamps (unix ms), anchored to yesterday noon UTC: always inside
// the 30-day reporting window, and far enough from midnight that both jobs
// always land on the same UTC day.
var (
	anchor = time.Now().UTC().Truncate(24 * time.Hour).Add(-12 * time.Hour)

	tCreate = anchor.UnixMilli()                       // pid-ok queued
	tStart  = anchor.Add(4 * time.Second).UnixMilli()  // 4s waiting
	tEnd    = anchor.Add(12 * time.Second).UnixMilli() // 8s running
	tStart2 = anchor.Add(20 * time.Second).UnixMilli() // pid-err starts
	tEnd2   = anchor.Add(23 * time.Second).UnixMilli() // 3s running
)

const queueJSON = `{
  "queue_running": [[0, "pid-running", {"1": {}}, {}, []]],
  "queue_pending": [
    [1, "pid-pending-a", {"1": {}}, {}, []],
    [2, "pid-pending-b", {"1": {}}, {}, []]
  ]
}`

const statsJSON = `{
  "system": {
    "os": "posix",
    "ram_total": 1000,
    "ram_free": 250,
    "comfyui_version": "0.3.60",
    "pytorch_version": "2.6.0"
  },
  "devices": [
    {"name": "NVIDIA GeForce RTX 4090", "type": "cuda", "index": 0,
     "vram_total": 1000, "vram_free": 400,
     "torch_vram_total": 800, "torch_vram_free": 200}
  ]
}`

// historyJSON covers the shapes that matter: a success with cached nodes and
// two images (plus a non-media "animated" list that must not be counted), and
// a failure with no create_time.
var historyJSON = fmt.Sprintf(`{
  "pid-ok": {
    "prompt": [0, "pid-ok", {"1":{},"2":{},"3":{},"4":{}},
      {"create_time": %d, "extra_pnginfo": {"workflow": {"id": "wf-alpha"}}},
      ["4"]],
    "outputs": {
      "4": {"images": [{"filename":"a.png"},{"filename":"b.png"}], "animated": [false]}
    },
    "status": {
      "status_str": "success",
      "completed": true,
      "messages": [
        ["execution_start",   {"prompt_id":"pid-ok","timestamp":%d}],
        ["execution_cached",  {"nodes":["1","2"],"prompt_id":"pid-ok","timestamp":%d}],
        ["execution_success", {"prompt_id":"pid-ok","timestamp":%d}]
      ]
    }
  },
  "pid-err": {
    "prompt": [1, "pid-err", {"1":{},"2":{}}, {}, ["2"]],
    "outputs": {},
    "status": {
      "status_str": "error",
      "completed": false,
      "messages": [
        ["execution_start", {"prompt_id":"pid-err","timestamp":%d}],
        ["execution_error", {"prompt_id":"pid-err","node_id":"2",
                             "exception_message":"OOM","timestamp":%d}]
      ]
    }
  }
}`, tCreate, tStart, tStart, tEnd, tStart2, tEnd2)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/queue", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, queueJSON)
	})
	mux.HandleFunc("/system_stats", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, statsJSON)
	})
	mux.HandleFunc("/history", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("max_items") == "" {
			t.Errorf("history called without max_items: %s", r.URL)
		}
		io.WriteString(w, historyJSON)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newTestPoller(t *testing.T, base string) (*Poller, *storage.DB) {
	t.Helper()
	db, err := storage.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.New: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(base, db, logger), db
}

func TestSampleGauges(t *testing.T) {
	srv := newTestServer(t)
	p, db := newTestPoller(t, srv.URL)

	if err := p.SampleGauges(context.Background()); err != nil {
		t.Fatalf("SampleGauges: %v", err)
	}

	live := p.Now()
	if !live.Reachable {
		t.Fatalf("expected reachable, got error %q", live.Error)
	}
	if live.QueueRunning != 1 || live.QueuePending != 2 || live.QueueDepth != 3 {
		t.Errorf("queue = running %d pending %d depth %d; want 1/2/3",
			live.QueueRunning, live.QueuePending, live.QueueDepth)
	}
	if live.ComfyUIVersion != "0.3.60" {
		t.Errorf("comfyui_version = %q", live.ComfyUIVersion)
	}
	if live.RAMUsedPct != 75 {
		t.Errorf("ram_used_pct = %v; want 75", live.RAMUsedPct)
	}
	if len(live.VRAM) != 1 {
		t.Fatalf("devices = %d; want 1", len(live.VRAM))
	}
	d := live.VRAM[0]
	if d.Label != "cuda:0" {
		t.Errorf("label = %q; want cuda:0", d.Label)
	}
	if d.UsedPct != 60 {
		t.Errorf("vram used = %v; want 60", d.UsedPct)
	}
	if d.TorchUsedPct != 75 {
		t.Errorf("torch vram used = %v; want 75", d.TorchUsedPct)
	}

	// Gauges must land in the metrics table so the 30-day series has data.
	buckets, err := db.GetMetricBuckets(Source, "queue_depth", "", 1)
	if err != nil {
		t.Fatalf("GetMetricBuckets: %v", err)
	}
	if len(buckets) != 1 || buckets[0].Max != 3 {
		t.Errorf("queue_depth buckets = %+v; want one bucket with max 3", buckets)
	}
	labels, err := db.MetricLabels(Source, "vram_used_pct", 1)
	if err != nil {
		t.Fatalf("MetricLabels: %v", err)
	}
	if len(labels) != 1 || labels[0] != "cuda:0" {
		t.Errorf("vram labels = %v; want [cuda:0]", labels)
	}
}

func TestSampleGaugesUnreachable(t *testing.T) {
	// A dead ComfyUI must surface as an error and a not-reachable Live rather
	// than a panic or a silently zeroed sample.
	p, _ := newTestPoller(t, "http://127.0.0.1:1")
	err := p.SampleGauges(context.Background())
	if err == nil {
		t.Fatal("expected an error from an unreachable host")
	}
	live := p.Now()
	if live.Reachable {
		t.Error("Reachable should be false")
	}
	if live.Error == "" {
		t.Error("Error should describe the failure")
	}
}

func TestDrainJobs(t *testing.T) {
	srv := newTestServer(t)
	p, db := newTestPoller(t, srv.URL)

	if err := p.DrainJobs(context.Background()); err != nil {
		t.Fatalf("DrainJobs: %v", err)
	}

	overall, byDay, err := db.GetComfyStats(30)
	if err != nil {
		t.Fatalf("GetComfyStats: %v", err)
	}
	if overall.Jobs != 2 {
		t.Errorf("jobs = %d; want 2", overall.Jobs)
	}
	if overall.OK != 1 || overall.Failed != 1 {
		t.Errorf("ok/failed = %d/%d; want 1/1", overall.OK, overall.Failed)
	}
	// "animated": [false] must not inflate the image count.
	if overall.Images != 2 {
		t.Errorf("images = %d; want 2", overall.Images)
	}
	// pid-ok ran 8s, pid-err ran 3s → p50 (nearest-rank) is the 1st of 2 sorted.
	if overall.ExecP50Ms != tEnd2-tStart2 {
		t.Errorf("exec p50 = %d; want %d", overall.ExecP50Ms, tEnd2-tStart2)
	}
	if overall.ExecP95Ms != tEnd-tStart {
		t.Errorf("exec p95 = %d; want %d", overall.ExecP95Ms, tEnd-tStart)
	}
	// Only pid-ok has create_time, so it is the only wait sample: 4s.
	if overall.WaitP95Ms != tStart-tCreate {
		t.Errorf("wait p95 = %d; want %d", overall.WaitP95Ms, tStart-tCreate)
	}
	if overall.WaitEstimated {
		t.Error("wait should not be flagged estimated when create_time is present")
	}
	// 2 of 6 nodes across both jobs came from cache.
	if overall.CacheHitRatio != 0.333 {
		t.Errorf("cache hit ratio = %v; want 0.333", overall.CacheHitRatio)
	}
	if len(byDay) != 1 {
		t.Errorf("byDay = %d entries; want 1", len(byDay))
	}
}

func TestDrainJobsIsIdempotent(t *testing.T) {
	srv := newTestServer(t)
	p, db := newTestPoller(t, srv.URL)

	for i := 0; i < 3; i++ {
		if err := p.DrainJobs(context.Background()); err != nil {
			t.Fatalf("DrainJobs #%d: %v", i, err)
		}
	}
	overall, _, err := db.GetComfyStats(30)
	if err != nil {
		t.Fatalf("GetComfyStats: %v", err)
	}
	if overall.Jobs != 2 || overall.Images != 2 {
		t.Errorf("after 3 drains: jobs = %d, images = %d; want 2 and 2",
			overall.Jobs, overall.Images)
	}
}

func TestWaitTimeFallsBackToQueueObservation(t *testing.T) {
	// pid-err carries no create_time. If the sampler saw it waiting in the
	// queue first, the wait time is derived from that observation instead.
	srv := newTestServer(t)
	p, db := newTestPoller(t, srv.URL)

	seenAt := time.UnixMilli(tStart2 - 6000)
	p.mu.Lock()
	p.seen["pid-err"] = seenAt
	p.mu.Unlock()

	if err := p.DrainJobs(context.Background()); err != nil {
		t.Fatalf("DrainJobs: %v", err)
	}
	overall, _, err := db.GetComfyStats(30)
	if err != nil {
		t.Fatalf("GetComfyStats: %v", err)
	}
	if !overall.WaitEstimated {
		t.Error("expected WaitEstimated to be set")
	}
	if overall.WaitP95Ms != 6000 {
		t.Errorf("wait p95 = %d; want 6000 (the estimated one, larger than pid-ok's 4000)",
			overall.WaitP95Ms)
	}
	// The prompt is resolved, so it must not linger in the observation map.
	p.mu.RLock()
	_, still := p.seen["pid-err"]
	p.mu.RUnlock()
	if still {
		t.Error("resolved prompt should be dropped from the seen map")
	}
}

func TestObserveQueuedRecordsPendingPrompts(t *testing.T) {
	srv := newTestServer(t)
	p, _ := newTestPoller(t, srv.URL)

	if err := p.SampleGauges(context.Background()); err != nil {
		t.Fatalf("SampleGauges: %v", err)
	}
	first, ok := p.firstSeen("pid-pending-a")
	if !ok {
		t.Fatal("pending prompt was not recorded")
	}
	if _, ok := p.firstSeen("pid-running"); ok {
		t.Error("running prompts are not waiting and must not be recorded")
	}

	// A second sample must not move the first-seen timestamp forward, or every
	// wait estimate would collapse to zero.
	time.Sleep(5 * time.Millisecond)
	if err := p.SampleGauges(context.Background()); err != nil {
		t.Fatalf("SampleGauges #2: %v", err)
	}
	again, _ := p.firstSeen("pid-pending-a")
	if !again.Equal(first) {
		t.Errorf("first-seen moved from %v to %v", first, again)
	}
}

func TestCountOutputs(t *testing.T) {
	var outputs map[string]json.RawMessage
	raw := `{"9":{"images":[{"f":"a"}],"gifs":[{"f":"b"}],"animated":[true],"text":["hi"]},
	         "10":{"videos":[{"f":"c"}],"audio":[{"f":"d"}]}}`
	if err := json.Unmarshal([]byte(raw), &outputs); err != nil {
		t.Fatal(err)
	}
	if got := countOutputs(outputs); got != 4 {
		t.Errorf("countOutputs = %d; want 4 (images+gifs+videos+audio, not animated/text)", got)
	}
}
