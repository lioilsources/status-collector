// Command seed-demo fills a throwaway database with plausible data and writes
// the snapshot files, so the frontend can be exercised without a live NAS.
// It is a development scratch tool, not part of the deployed binary.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"time"

	"github.com/ol1n/status-collector/internal/api"
	"github.com/ol1n/status-collector/internal/checker"
	"github.com/ol1n/status-collector/internal/comfy"
	"github.com/ol1n/status-collector/internal/storage"
)

func main() {
	dbPath := flag.String("db", "/tmp/demo-status.db", "database to create")
	outDir := flag.String("out", "/tmp/demo-snapshot", "snapshot output directory")
	flag.Parse()

	_ = os.Remove(*dbPath)
	db, err := storage.New(*dbPath)
	if err != nil {
		panic(err)
	}
	defer db.Close()

	rnd := rand.New(rand.NewSource(42))
	now := time.Now().UTC().Truncate(time.Hour)
	endpoints := checker.DefaultEndpoints("http://127.0.0.1:18188")

	// ── availability: 30 days of hourly checks, with two outage windows ──
	for _, ep := range endpoints {
		for h := 30 * 24; h >= 0; h-- {
			ts := now.Add(-time.Duration(h) * time.Hour)
			up := true
			switch {
			case h >= 200 && h < 209 && ep.Group != "Health":
				up = false
			case h >= 41 && h < 44:
				up = false
			case rnd.Float64() < 0.004:
				up = false
			}
			code := 200
			if !up {
				code = 502
			}
			errMsg := ""
			if !up {
				errMsg = "upstream unavailable"
			}
			if err := db.InsertCheck(storage.Check{
				EndpointID: ep.ID,
				Timestamp:  ts,
				StatusCode: code,
				LatencyMs:  int64(40 + rnd.Intn(180)),
				Up:         up,
				Error:      errMsg,
			}); err != nil {
				panic(err)
			}
		}
	}

	// ── ComfyUI gauges: one sample per minute is too much for a demo, so
	//    sample every 10 minutes across the window ──
	for m := 30 * 24 * 6; m >= 0; m-- {
		ts := now.Add(-time.Duration(m) * 10 * time.Minute)
		hourOfDay := ts.Hour()
		// busier in the evening
		load := 0.0
		if hourOfDay >= 17 && hourOfDay <= 23 {
			load = rnd.Float64() * 9
		} else if rnd.Float64() < 0.25 {
			load = rnd.Float64() * 3
		}
		depth := float64(int(load))
		running := 0.0
		if depth > 0 {
			running = 1
		}
		vram := 12 + load*8 + rnd.Float64()*6
		if vram > 99 {
			vram = 99
		}
		if err := db.InsertMetrics(comfy.Source, ts, []storage.Metric{
			{Name: "queue_depth", Value: depth},
			{Name: "queue_running", Value: running},
			{Name: "queue_pending", Value: depth - running},
			{Name: "ram_used_pct", Value: 45 + rnd.Float64()*25},
			{Name: "vram_used_pct", Label: "cuda:0", Value: vram},
		}); err != nil {
			panic(err)
		}
	}

	// ── ComfyUI jobs ──
	jobs := 0
	images := 0
	for d := 29; d >= 0; d-- {
		day := now.AddDate(0, 0, -d)
		count := rnd.Intn(28)
		if d%7 == 0 {
			count = rnd.Intn(6)
		}
		for i := 0; i < count; i++ {
			created := time.Date(day.Year(), day.Month(), day.Day(), 8+rnd.Intn(14), rnd.Intn(60), rnd.Intn(60), 0, time.UTC)
			if created.After(now) {
				continue
			}
			wait := time.Duration(rnd.Intn(45)) * time.Second
			exec := time.Duration(4+rnd.Intn(90)) * time.Second
			status := "success"
			out := 1 + rnd.Intn(4)
			errMsg := ""
			if rnd.Float64() < 0.06 {
				status = "error"
				out = 0
				errMsg = "CUDA out of memory"
			}
			nodes := 12 + rnd.Intn(20)
			if err := db.UpsertComfyJob(storage.ComfyJob{
				PromptID:    fmt.Sprintf("demo-%d-%d", d, i),
				CreatedAt:   created.UnixMilli(),
				StartedAt:   created.Add(wait).UnixMilli(),
				EndedAt:     created.Add(wait + exec).UnixMilli(),
				Status:      status,
				Outputs:     out,
				NodesTotal:  nodes,
				NodesCached: rnd.Intn(nodes),
				WorkflowID:  "wf-demo",
				Error:       errMsg,
			}); err != nil {
				panic(err)
			}
			jobs++
			images += out
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	poller := comfy.New("http://127.0.0.1:18188", db, logger)
	// Best-effort live sample; the demo works without a ComfyUI running.
	_ = poller.SampleGauges(context.Background())

	srv := api.New(db, logger, endpoints, poller)
	if err := srv.WriteSnapshot(*outDir); err != nil {
		panic(err)
	}
	fmt.Printf("seeded %d endpoints, %d jobs, %d images -> %s\n", len(endpoints), jobs, images, *outDir)
}
