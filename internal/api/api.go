package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/ol1n/status-collector/internal/checker"
	"github.com/ol1n/status-collector/internal/storage"
)

type Server struct {
	db        *storage.DB
	logger    *slog.Logger
	endpoints []checker.Endpoint
	mux       *http.ServeMux
}

func New(db *storage.DB, logger *slog.Logger) *Server {
	s := &Server{
		db:        db,
		logger:    logger,
		endpoints: checker.DefaultEndpoints(),
		mux:       http.NewServeMux(),
	}
	s.mux.HandleFunc("GET /api/status", s.handleStatus)
	s.mux.HandleFunc("GET /api/history/{endpointID}", s.handleHistory)
	s.mux.Handle("GET /", http.FileServer(http.Dir("/var/lib/ol1n-status/web")))
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

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := StatusResponse{GeneratedAt: time.Now().UTC()}

	for _, ep := range s.endpoints {
		es := EndpointStatus{
			ID:    ep.ID,
			Name:  ep.Name,
			Group: ep.Group,
			URL:   ep.URL,
		}

		// Latest check
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

		// Hourly buckets for the last 30 days
		buckets, err := s.db.GetHourBuckets(ep.ID, 30)
		if err != nil {
			s.logger.Warn("get buckets", "endpoint", ep.ID, "err", err)
		}

		var totalUp, totalChecks int
		for _, b := range buckets {
			total := b.TotalUp + b.TotalDown
			avail := -1.0
			if total > 0 {
				avail = float64(b.TotalUp) / float64(total)
				totalUp += b.TotalUp
				totalChecks += total
			}
			es.Buckets = append(es.Buckets, HourBucketResponse{
				Hour:     b.Hour,
				Up:       b.TotalUp,
				Down:     b.TotalDown,
				AvgLatMs: b.AvgLatMs,
				Avail:    avail,
			})
		}
		if totalChecks > 0 {
			es.Uptime30 = float64(totalUp) / float64(totalChecks) * 100
		}

		resp.Endpoints = append(resp.Endpoints, es)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	epID := r.PathValue("endpointID")
	buckets, err := s.db.GetHourBuckets(epID, 30)
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(buckets)
}
