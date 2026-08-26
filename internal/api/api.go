package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ol1n/status-collector/internal/checker"
	"github.com/ol1n/status-collector/internal/comfy"
	"github.com/ol1n/status-collector/internal/storage"
)

// days is the reporting window shared by the status grid and the ComfyUI series.
const days = 30

type Server struct {
	db        *storage.DB
	logger    *slog.Logger
	endpoints []checker.Endpoint
	comfy     *comfy.Poller // nil when ComfyUI monitoring is disabled
	mux       *http.ServeMux
}

// New builds the API. The static frontend is not served here — it lives on
// GitHub Pages and reaches this API cross-origin.
func New(db *storage.DB, logger *slog.Logger, endpoints []checker.Endpoint, poller *comfy.Poller) *Server {
	s := &Server{
		db:        db,
		logger:    logger,
		endpoints: endpoints,
		comfy:     poller,
		mux:       http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/comfy", s.handleComfy)
	s.mux.HandleFunc("GET /api/history/{endpointID}", s.handleHistory)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	s.mux.ServeHTTP(w, r)
}

// StatusResponse is the full payload consumed by the frontend.
type StatusResponse struct {
	GeneratedAt time.Time        `json:"generated_at"`
	Endpoints   []EndpointStatus `json:"endpoints"`
}

type EndpointStatus struct {
	ID       string               `json:"id"`
	Name     string               `json:"name"`
	Group    string               `json:"group"`
	URL      string               `json:"url"`
	Current  *CurrentStatus       `json:"current"`
	Uptime30 float64              `json:"uptime_30d"` // percent
	Buckets  []HourBucketResponse `json:"buckets"`    // last 30 days hourly
}

type CurrentStatus struct {
	Up         bool      `json:"up"`
	StatusCode int       `json:"status_code"`
	LatencyMs  int64     `json:"latency_ms"`
	CheckedAt  time.Time `json:"checked_at"`
	Error      string    `json:"error,omitempty"`
}

type HourBucketResponse struct {
	Hour     time.Time `json:"hour"`
	Up       int       `json:"up"`
	Down     int       `json:"down"`
	AvgLatMs float64   `json:"avg_lat_ms"`
	// availability: 0.0-1.0; -1 = no data
	Avail float64 `json:"avail"`
}

// ComfyResponse is the ComfyUI metrics payload. It is separate from
// /api/status so the availability grid keeps its existing wire format.
type ComfyResponse struct {
	GeneratedAt time.Time          `json:"generated_at"`
	Enabled     bool               `json:"enabled"`
	Now         *comfy.Live        `json:"now,omitempty"`
	QueueDepth  []MetricPoint      `json:"queue_depth"`
	VRAM        []LabeledSeries    `json:"vram_used_pct"`
	RAM         []MetricPoint      `json:"ram_used_pct"`
	Days        []storage.DayStats `json:"days"`
	Totals      ComfyTotals        `json:"totals"`
}

type MetricPoint struct {
	Hour time.Time `json:"hour"`
	Avg  float64   `json:"avg"`
	Max  float64   `json:"max"`
}

type LabeledSeries struct {
	Label  string        `json:"label"`
	Name   string        `json:"name,omitempty"`
	Points []MetricPoint `json:"points"`
}

type ComfyTotals struct {
	Jobs          int     `json:"jobs"`
	Images        int     `json:"images"`
	ImagesToday   int     `json:"images_today"`
	Failed        int     `json:"failed"`
	Cancelled     int     `json:"cancelled"`
	ExecP50Ms     int64   `json:"exec_p50_ms"`
	ExecP95Ms     int64   `json:"exec_p95_ms"`
	WaitP50Ms     int64   `json:"wait_p50_ms"`
	WaitP95Ms     int64   `json:"wait_p95_ms"`
	WaitEstimated bool    `json:"wait_estimated"`
	CacheHitRatio float64 `json:"cache_hit_ratio"`
}

// ── Handlers ────────────────────────────────────────────────────────────────

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.buildStatus())
}

func (s *Server) handleComfy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.buildComfy())
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	epID := r.PathValue("endpointID")
	buckets, err := s.db.GetHourBuckets(epID, days)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	// Mirror the /api/status bucket shape rather than leaking the Go struct's
	// PascalCase field names.
	out := make([]HourBucketResponse, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, toBucket(b))
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ── Payload builders ────────────────────────────────────────────────────────

func (s *Server) buildStatus() StatusResponse {
	resp := StatusResponse{GeneratedAt: time.Now().UTC()}

	for _, ep := range s.endpoints {
		es := EndpointStatus{
			ID:    ep.ID,
			Name:  ep.Name,
			Group: ep.Group,
			URL:   ep.URL,
		}

		latest, err := s.db.GetLatestCheck(ep.ID)
		if err != nil {
			s.logger.Warn("get latest check", "endpoint", ep.ID, "err", err)
		}
		if latest != nil {
			es.Current = &CurrentStatus{
				Up:         latest.Up,
				StatusCode: latest.StatusCode,
				LatencyMs:  latest.LatencyMs,
				CheckedAt:  latest.Timestamp,
				Error:      latest.Error,
			}
		}

		buckets, err := s.db.GetHourBuckets(ep.ID, days)
		if err != nil {
			s.logger.Warn("get buckets", "endpoint", ep.ID, "err", err)
		}

		var totalUp, totalChecks int
		for _, b := range buckets {
			total := b.TotalUp + b.TotalDown
			if total > 0 {
				totalUp += b.TotalUp
				totalChecks += total
			}
			es.Buckets = append(es.Buckets, toBucket(b))
		}
		if totalChecks > 0 {
			es.Uptime30 = round(float64(totalUp)/float64(totalChecks)*100, 3)
		}

		resp.Endpoints = append(resp.Endpoints, es)
	}
	return resp
}

func toBucket(b storage.HourBucket) HourBucketResponse {
	total := b.TotalUp + b.TotalDown
	avail := -1.0
	if total > 0 {
		avail = round(float64(b.TotalUp)/float64(total), 3)
	}
	return HourBucketResponse{
		Hour:     b.Hour,
		Up:       b.TotalUp,
		Down:     b.TotalDown,
		AvgLatMs: round(b.AvgLatMs, 0),
		Avail:    avail,
	}
}

func (s *Server) buildComfy() ComfyResponse {
	resp := ComfyResponse{
		GeneratedAt: time.Now().UTC(),
		Enabled:     s.comfy != nil,
		QueueDepth:  []MetricPoint{},
		VRAM:        []LabeledSeries{},
		RAM:         []MetricPoint{},
		Days:        []storage.DayStats{},
	}
	if s.comfy == nil {
		return resp
	}

	live := s.comfy.Now()
	resp.Now = &live

	resp.QueueDepth = s.series("queue_depth", "")
	resp.RAM = s.series("ram_used_pct", "")

	labels, err := s.db.MetricLabels(comfy.Source, "vram_used_pct", days)
	if err != nil {
		s.logger.Warn("vram labels", "err", err)
	}
	// Device names only exist in the live sample; fall back to the bare label
	// when ComfyUI is unreachable right now.
	names := map[string]string{}
	for _, d := range live.VRAM {
		names[d.Label] = d.Name
	}
	for _, label := range labels {
		resp.VRAM = append(resp.VRAM, LabeledSeries{
			Label:  label,
			Name:   names[label],
			Points: s.series("vram_used_pct", label),
		})
	}

	overall, byDay, err := s.db.GetComfyStats(days)
	if err != nil {
		s.logger.Warn("comfy stats", "err", err)
		return resp
	}
	if byDay != nil {
		resp.Days = byDay
	}
	resp.Totals = ComfyTotals{
		Jobs:          overall.Jobs,
		Images:        overall.Images,
		Failed:        overall.Failed,
		Cancelled:     overall.Cancelled,
		ExecP50Ms:     overall.ExecP50Ms,
		ExecP95Ms:     overall.ExecP95Ms,
		WaitP50Ms:     overall.WaitP50Ms,
		WaitP95Ms:     overall.WaitP95Ms,
		WaitEstimated: overall.WaitEstimated,
		CacheHitRatio: overall.CacheHitRatio,
	}
	today := time.Now().UTC().Format("2006-01-02")
	for _, d := range byDay {
		if d.Day == today {
			resp.Totals.ImagesToday = d.Images
		}
	}
	return resp
}

func (s *Server) series(name, label string) []MetricPoint {
	buckets, err := s.db.GetMetricBuckets(comfy.Source, name, label, days)
	if err != nil {
		s.logger.Warn("metric buckets", "name", name, "label", label, "err", err)
		return []MetricPoint{}
	}
	out := make([]MetricPoint, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, MetricPoint{
			Hour: b.Hour,
			Avg:  round(b.Avg, 2),
			Max:  round(b.Max, 2),
		})
	}
	return out
}

// ── Snapshot ────────────────────────────────────────────────────────────────

// WriteSnapshot dumps the same payloads the HTTP handlers serve into dir, so a
// static frontend can fall back to them when this host is unreachable.
func (s *Server) WriteSnapshot(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir snapshot dir: %w", err)
	}
	if err := writeJSONFile(filepath.Join(dir, "status.json"), s.buildStatus()); err != nil {
		return err
	}
	return writeJSONFile(filepath.Join(dir, "comfy.json"), s.buildComfy())
}

// writeJSONFile writes atomically — the publish script may read the directory
// at any moment and must never pick up a half-written file.
func writeJSONFile(path string, v any) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create %s: %w", tmp, err)
	}
	if err := json.NewEncoder(f).Encode(v); err != nil {
		f.Close()
		return fmt.Errorf("encode %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

// round keeps the snapshot small and its diffs stable between publishes.
func round(v float64, decimals int) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	f := math.Pow(10, float64(decimals))
	return math.Round(v*f) / f
}
