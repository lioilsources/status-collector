package checker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/ol1n/status-collector/internal/storage"
)

// Endpoint describes a single monitored endpoint.
type Endpoint struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Group       string            `json:"group"`
	URL         string            `json:"url"`
	Method      string            `json:"method"`
	Body        string            `json:"body,omitempty"`
	ContentType string            `json:"content_type,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	// ExpectStatus: if 0, any 2xx counts as up.
	ExpectStatus int `json:"expect_status,omitempty"`
}

// cfAccessHeaders returns Cloudflare Access service-token headers from environment variables.
func cfAccessHeaders() map[string]string {
	h := map[string]string{}
	if id := os.Getenv("CF_ACCESS_CLIENT_ID"); id != "" {
		h["CF-Access-Client-Id"] = id
	}
	if secret := os.Getenv("CF_ACCESS_CLIENT_SECRET"); secret != "" {
		h["CF-Access-Client-Secret"] = secret
	}
	return h
}

// DefaultEndpoints returns the standard monitored endpoints.
func DefaultEndpoints() []Endpoint {
	base := "https://llm.ol1n.com"
	cf := cfAccessHeaders()
	return []Endpoint{
		// ── Health ─────────────────────────────────────────────────────────
		{
			ID:    "health",
			Name:  "vLLM Health",
			Group: "Health",
			URL:   base + "/health",
		},
		{
			ID:    "ping",
			Name:  "API Ping",
			Group: "Health",
			URL:   base + "/ping",
		},
		// ── OpenAI-compatible ──────────────────────────────────────────────
		{
			ID:    "openai_models",
			Name:  "GET /v1/models",
			Group: "OpenAI API",
			URL:   base + "/v1/models",
		},
		{
			ID:          "openai_chat",
			Name:        "POST /v1/chat/completions",
			Group:       "OpenAI API",
			URL:         base + "/v1/chat/completions",
			Method:      "POST",
			ContentType: "application/json",
			Body: `{
				"model":"__auto__",
				"max_tokens":1,
				"messages":[{"role":"user","content":"ping"}]
			}`,
			ExpectStatus: 200,
		},
		{
			ID:          "openai_completions",
			Name:        "POST /v1/completions",
			Group:       "OpenAI API",
			URL:         base + "/v1/completions",
			Method:      "POST",
			ContentType: "application/json",
			Body:        `{"model":"__auto__","max_tokens":1,"prompt":"ping"}`,
			ExpectStatus: 200,
		},
		{
			ID:          "openai_embeddings",
			Name:        "POST /v1/embeddings",
			Group:       "OpenAI API",
			URL:         base + "/v1/embeddings",
			Method:      "POST",
			ContentType: "application/json",
			Body:        `{"model":"__auto__","input":"ping"}`,
			// Embeddings may not be served — 422/404 still means the API layer is alive
			ExpectStatus: 0,
		},
		// ── vLLM-specific ──────────────────────────────────────────────────
		{
			ID:    "vllm_version",
			Name:  "GET /version",
			Group: "vLLM",
			URL:   base + "/version",
		},
		{
			ID:    "vllm_metrics",
			Name:  "GET /metrics",
			Group: "vLLM",
			URL:   base + "/metrics",
		},
		{
			ID:    "vllm_tokenize",
			Name:  "GET /tokenize",
			Group: "vLLM",
			URL:   base + "/tokenize?text=ping",
		},
		// ── ComfyUI ────────────────────────────────────────────────────────
		{
			ID:      "comfyui_stats",
			Name:    "ComfyUI System Stats",
			Group:   "ComfyUI",
			URL:     "https://comfyui.ol1n.com/system_stats",
			Headers: cf,
		},
		{
			ID:      "comfyui_queue",
			Name:    "ComfyUI Queue",
			Group:   "ComfyUI",
			URL:     "https://comfyui.ol1n.com/queue",
			Headers: cf,
		},
		// ── Sonarr ─────────────────────────────────────────────────────────
		{
			ID:    "sonarr_ping",
			Name:  "Sonarr Ping",
			Group: "Sonarr",
			URL:   "http://localhost:8989/ping",
		},
		// ── Radarr ─────────────────────────────────────────────────────────
		{
			ID:    "radarr_ping",
			Name:  "Radarr Ping",
			Group: "Radarr",
			URL:   "http://localhost:7878/ping",
		},
	}
}

// Checker runs health checks against all endpoints.
type Checker struct {
	endpoints []Endpoint
	db        *storage.DB
	logger    *slog.Logger
	client    *http.Client
}

func New(endpoints []Endpoint, db *storage.DB, logger *slog.Logger) *Checker {
	return &Checker{
		endpoints: endpoints,
		db:        db,
		logger:    logger,
		client: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

func (c *Checker) RunOnce(ctx context.Context) {
	for _, ep := range c.endpoints {
		select {
		case <-ctx.Done():
			return
		default:
		}
		result := c.checkEndpoint(ctx, ep)
		if err := c.db.InsertCheck(result); err != nil {
			c.logger.Error("insert check failed", "endpoint", ep.ID, "err", err)
		}
		c.logger.Info("checked",
			"endpoint", ep.ID,
			"up", result.Up,
			"status", result.StatusCode,
			"latency_ms", result.LatencyMs,
		)
	}
	// Prune records older than 35 days
	if err := c.db.PruneOld(35); err != nil {
		c.logger.Warn("prune failed", "err", err)
	}
}

func (c *Checker) checkEndpoint(ctx context.Context, ep Endpoint) storage.Check {
	method := ep.Method
	if method == "" {
		method = http.MethodGet
	}

	body := ep.Body
	// Replace __auto__ with first available model if needed — best-effort
	if strings.Contains(body, "__auto__") {
		if model := c.fetchFirstModel(ctx); model != "" {
			body = strings.ReplaceAll(body, "__auto__", model)
		}
	}

	var reqBody io.Reader
	if body != "" {
		reqBody = strings.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, ep.URL, reqBody)
	if err != nil {
		return storage.Check{
			EndpointID: ep.ID,
			Timestamp:  time.Now().UTC(),
			Error:      fmt.Sprintf("build request: %v", err),
		}
	}
	if ep.ContentType != "" {
		req.Header.Set("Content-Type", ep.ContentType)
	}
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}

	start := time.Now()
	resp, err := c.client.Do(req)
	elapsed := time.Since(start).Milliseconds()

	if err != nil {
		return storage.Check{
			EndpointID: ep.ID,
			Timestamp:  time.Now().UTC(),
			LatencyMs:  elapsed,
			Error:      err.Error(),
		}
	}
	defer resp.Body.Close()
	// Drain body so connection can be reused
	_, _ = io.Copy(io.Discard, resp.Body)

	up := isUp(resp.StatusCode, ep.ExpectStatus)
	return storage.Check{
		EndpointID: ep.ID,
		Timestamp:  time.Now().UTC(),
		StatusCode: resp.StatusCode,
		LatencyMs:  elapsed,
		Up:         up,
	}
}

// fetchFirstModel calls /v1/models and returns the first model id, or empty string on failure.
func (c *Checker) fetchFirstModel(ctx context.Context) string {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://llm.ol1n.com/v1/models", nil)
	resp, err := c.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return ""
	}
	if len(result.Data) > 0 {
		return result.Data[0].ID
	}
	return ""
}

func isUp(code, expect int) bool {
	if expect != 0 {
		return code == expect
	}
	return code >= 200 && code < 300
}
