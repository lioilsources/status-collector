// Package comfy polls a ComfyUI instance for queue, throughput and hardware
// metrics.
//
// Everything here comes from three plain GET endpoints — /queue, /system_stats
// and /history. ComfyUI also exposes /api/jobs on newer builds (which hands out
// create_time and execution_duration ready-made) and a /ws stream with
// per-node progress, but /history is the one source that carries *all* of the
// metrics we want — timestamps, outputs, node counts and cache hits — and it
// exists on every version, so it is the only job source used.
package comfy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ol1n/status-collector/internal/storage"
)

// Source is the value written to the metrics table's source column.
const Source = "comfyui"

// historyPageSize is how many recent history entries each drain reads. ComfyUI
// returns the *last* N entries for max_items, so a full page of previously
// unseen jobs means the drain interval is too long — DrainJobs logs that.
const historyPageSize = 100

// seenTTL bounds the queue-observation map used to estimate wait time when
// ComfyUI does not report create_time.
const seenTTL = 12 * time.Hour

// Device is one GPU as reported by /system_stats.
type Device struct {
	Label        string  `json:"label"` // "cuda:0"
	Name         string  `json:"name"`
	UsedPct      float64 `json:"used_pct"`
	TorchUsedPct float64 `json:"torch_used_pct"`
	TotalBytes   int64   `json:"total_bytes"`
	FreeBytes    int64   `json:"free_bytes"`
}

// Live is the most recent gauge sample, served as the "now" block of /api/comfy.
type Live struct {
	SampledAt      time.Time `json:"sampled_at"`
	Reachable      bool      `json:"reachable"`
	Error          string    `json:"error,omitempty"`
	QueueRunning   int       `json:"queue_running"`
	QueuePending   int       `json:"queue_pending"`
	QueueDepth     int       `json:"queue_depth"`
	VRAM           []Device  `json:"vram"`
	RAMUsedPct     float64   `json:"ram_used_pct"`
	RAMTotalBytes  int64     `json:"ram_total_bytes"`
	ComfyUIVersion string    `json:"comfyui_version,omitempty"`
	PyTorchVersion string    `json:"pytorch_version,omitempty"`
}

// Poller samples one ComfyUI instance.
type Poller struct {
	base   string
	client *http.Client
	db     *storage.DB
	logger *slog.Logger

	mu   sync.RWMutex
	live Live
	// seen maps a queued prompt_id to when we first observed it waiting. It is
	// the fallback for queue-wait time on builds that do not set create_time.
	seen map[string]time.Time
}

func New(base string, db *storage.DB, logger *slog.Logger) *Poller {
	return &Poller{
		base:   strings.TrimSuffix(base, "/"),
		client: &http.Client{Timeout: 15 * time.Second},
		db:     db,
		logger: logger,
		seen:   map[string]time.Time{},
	}
}

// Now returns the last gauge sample.
func (p *Poller) Now() Live {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.live
}

func (p *Poller) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+path, nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// ── Gauges ──────────────────────────────────────────────────────────────────

type queueResponse struct {
	Running []json.RawMessage `json:"queue_running"`
	Pending []json.RawMessage `json:"queue_pending"`
}

type systemStats struct {
	System struct {
		RAMTotal       int64  `json:"ram_total"`
		RAMFree        int64  `json:"ram_free"`
		ComfyUIVersion string `json:"comfyui_version"`
		PyTorchVersion string `json:"pytorch_version"`
	} `json:"system"`
	Devices []struct {
		Name           string `json:"name"`
		Type           string `json:"type"`
		Index          *int   `json:"index"`
		VRAMTotal      int64  `json:"vram_total"`
		VRAMFree       int64  `json:"vram_free"`
		TorchVRAMTotal int64  `json:"torch_vram_total"`
		TorchVRAMFree  int64  `json:"torch_vram_free"`
	} `json:"devices"`
}

// SampleGauges reads /queue and /system_stats and stores one metrics sample.
func (p *Poller) SampleGauges(ctx context.Context) error {
	now := time.Now().UTC()
	live := Live{SampledAt: now}
	var metrics []storage.Metric
	var firstErr error

	var q queueResponse
	if err := p.get(ctx, "/queue", &q); err != nil {
		firstErr = err
	} else {
		live.Reachable = true
		live.QueueRunning = len(q.Running)
		live.QueuePending = len(q.Pending)
		live.QueueDepth = live.QueueRunning + live.QueuePending
		metrics = append(metrics,
			storage.Metric{Name: "queue_depth", Value: float64(live.QueueDepth)},
			storage.Metric{Name: "queue_running", Value: float64(live.QueueRunning)},
			storage.Metric{Name: "queue_pending", Value: float64(live.QueuePending)},
		)
		p.observeQueued(q, now)
	}

	var st systemStats
	if err := p.get(ctx, "/system_stats", &st); err != nil {
		if firstErr == nil {
			firstErr = err
		}
	} else {
		live.Reachable = true
		live.ComfyUIVersion = st.System.ComfyUIVersion
		live.PyTorchVersion = st.System.PyTorchVersion
		live.RAMTotalBytes = st.System.RAMTotal
		if st.System.RAMTotal > 0 {
			live.RAMUsedPct = pct(st.System.RAMTotal-st.System.RAMFree, st.System.RAMTotal)
			metrics = append(metrics, storage.Metric{Name: "ram_used_pct", Value: live.RAMUsedPct})
		}
		for i, d := range st.Devices {
			idx := i
			if d.Index != nil {
				idx = *d.Index
			}
			dev := Device{
				Label:      fmt.Sprintf("%s:%d", orDefault(d.Type, "dev"), idx),
				Name:       d.Name,
				TotalBytes: d.VRAMTotal,
				FreeBytes:  d.VRAMFree,
			}
			if d.VRAMTotal > 0 {
				dev.UsedPct = pct(d.VRAMTotal-d.VRAMFree, d.VRAMTotal)
				metrics = append(metrics, storage.Metric{
					Name: "vram_used_pct", Label: dev.Label, Value: dev.UsedPct,
				})
			}
			if d.TorchVRAMTotal > 0 {
				dev.TorchUsedPct = pct(d.TorchVRAMTotal-d.TorchVRAMFree, d.TorchVRAMTotal)
			}
			live.VRAM = append(live.VRAM, dev)
		}
	}

	if firstErr != nil {
		live.Error = firstErr.Error()
	}

	p.mu.Lock()
	p.live = live
	p.mu.Unlock()

	if err := p.db.InsertMetrics(Source, now, metrics); err != nil {
		return fmt.Errorf("insert metrics: %w", err)
	}
	return firstErr
}

// observeQueued records when each pending prompt was first seen waiting.
func (p *Poller) observeQueued(q queueResponse, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, raw := range q.Pending {
		if id := queueEntryPromptID(raw); id != "" {
			if _, ok := p.seen[id]; !ok {
				p.seen[id] = now
			}
		}
	}
	for id, t := range p.seen {
		if now.Sub(t) > seenTTL {
			delete(p.seen, id)
		}
	}
}

// queueEntryPromptID pulls prompt_id out of a queue tuple
// [number, prompt_id, prompt, extra_data, outputs_to_execute].
func queueEntryPromptID(raw json.RawMessage) string {
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) < 2 {
		return ""
	}
	var id string
	if err := json.Unmarshal(tuple[1], &id); err != nil {
		return ""
	}
	return id
}

// ── Jobs ────────────────────────────────────────────────────────────────────

type historyEntry struct {
	// prompt tuple: [number, prompt_id, prompt_graph, extra_data, outputs_to_execute]
	Prompt  []json.RawMessage          `json:"prompt"`
	Outputs map[string]json.RawMessage `json:"outputs"`
	Status  struct {
		StatusStr string            `json:"status_str"`
		Completed bool              `json:"completed"`
		Messages  []json.RawMessage `json:"messages"`
	} `json:"status"`
}

// mediaKeys are the /history output keys that hold generated artefacts. Node
// outputs also carry non-media lists (e.g. "animated": [true]), so counting
// every list would inflate the image count.
var mediaKeys = []string{"images", "gifs", "videos", "audio"}

// DrainJobs reads the most recent history page and upserts each job. It is
// idempotent — ComfyUI keeps history in memory only, so re-reading the same
// window on every drain is how we survive its restarts without double-counting.
func (p *Poller) DrainJobs(ctx context.Context) error {
	var hist map[string]historyEntry
	path := fmt.Sprintf("/history?max_items=%d", historyPageSize)
	if err := p.get(ctx, path, &hist); err != nil {
		return err
	}

	var newCount int
	for promptID, e := range hist {
		known, err := p.db.HasComfyJob(promptID)
		if err != nil {
			p.logger.Warn("comfy job lookup failed", "prompt_id", promptID, "err", err)
		}
		if !known {
			newCount++
		}
		job := p.buildJob(promptID, e)
		if err := p.db.UpsertComfyJob(job); err != nil {
			p.logger.Warn("upsert comfy job failed", "prompt_id", promptID, "err", err)
			continue
		}
		p.forget(promptID)
	}

	if newCount == len(hist) && len(hist) >= historyPageSize {
		p.logger.Warn("history page was entirely new — jobs may have been missed between drains",
			"page_size", historyPageSize)
	}
	p.logger.Info("comfy history drained", "entries", len(hist), "new", newCount)
	return nil
}

func (p *Poller) forget(promptID string) {
	p.mu.Lock()
	delete(p.seen, promptID)
	p.mu.Unlock()
}

func (p *Poller) firstSeen(promptID string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	t, ok := p.seen[promptID]
	return t, ok
}

func (p *Poller) buildJob(promptID string, e historyEntry) storage.ComfyJob {
	job := storage.ComfyJob{
		PromptID: promptID,
		Status:   e.Status.StatusStr,
		Outputs:  countOutputs(e.Outputs),
	}
	if job.Status == "" {
		if e.Status.Completed {
			job.Status = "success"
		} else {
			job.Status = "unknown"
		}
	}

	// prompt[2] is the workflow graph — its length is the node count, the
	// denominator of the cache hit ratio.
	if len(e.Prompt) > 2 {
		var graph map[string]json.RawMessage
		if err := json.Unmarshal(e.Prompt[2], &graph); err == nil {
			job.NodesTotal = len(graph)
		}
	}
	// prompt[3] is extra_data — newer frontends stamp create_time (unix ms) here.
	if len(e.Prompt) > 3 {
		var extra struct {
			CreateTime   *float64 `json:"create_time"`
			ExtraPnginfo struct {
				Workflow struct {
					ID string `json:"id"`
				} `json:"workflow"`
			} `json:"extra_pnginfo"`
		}
		if err := json.Unmarshal(e.Prompt[3], &extra); err == nil {
			if extra.CreateTime != nil {
				job.CreatedAt = int64(*extra.CreateTime)
			}
			job.WorkflowID = extra.ExtraPnginfo.Workflow.ID
		}
	}

	for _, raw := range e.Status.Messages {
		event, data := decodeMessage(raw)
		ts := int64(0)
		if data != nil {
			ts = data.Timestamp
		}
		switch event {
		case "execution_start":
			job.StartedAt = ts
		case "execution_cached":
			if data != nil {
				job.NodesCached = len(data.Nodes)
			}
		case "execution_success":
			job.EndedAt = ts
			job.Status = "success"
		case "execution_error":
			job.EndedAt = ts
			job.Status = "error"
			if data != nil {
				job.Error = data.ExceptionMessage
			}
		case "execution_interrupted":
			job.EndedAt = ts
			job.Status = "cancelled"
		}
	}

	// No create_time from ComfyUI — fall back to when we first saw the prompt
	// sitting in queue_pending. That is a lower bound, hence the flag.
	if job.CreatedAt == 0 && job.StartedAt > 0 {
		if t, ok := p.firstSeen(promptID); ok {
			job.CreatedAt = t.UnixMilli()
			job.WaitEstimated = true
		}
	}
	// A job that ran but never reported an end timestamp would fall out of the
	// day aggregation entirely; anchor it to its start instead.
	if job.EndedAt == 0 && job.StartedAt > 0 && e.Status.Completed {
		job.EndedAt = job.StartedAt
	}
	return job
}

type messageData struct {
	Timestamp        int64             `json:"timestamp"`
	Nodes            []json.RawMessage `json:"nodes"`
	ExceptionMessage string            `json:"exception_message"`
}

// decodeMessage unpacks a status message tuple [event, data].
func decodeMessage(raw json.RawMessage) (string, *messageData) {
	var tuple []json.RawMessage
	if err := json.Unmarshal(raw, &tuple); err != nil || len(tuple) == 0 {
		return "", nil
	}
	var event string
	if err := json.Unmarshal(tuple[0], &event); err != nil {
		return "", nil
	}
	if len(tuple) < 2 {
		return event, nil
	}
	var data messageData
	if err := json.Unmarshal(tuple[1], &data); err != nil {
		return event, nil
	}
	return event, &data
}

func countOutputs(outputs map[string]json.RawMessage) int {
	total := 0
	for _, raw := range outputs {
		var node map[string][]json.RawMessage
		if err := json.Unmarshal(raw, &node); err != nil {
			continue
		}
		for _, k := range mediaKeys {
			total += len(node[k])
		}
	}
	return total
}

func pct(part, total int64) float64 {
	if total <= 0 {
		return 0
	}
	v := float64(part) / float64(total) * 100
	return float64(int64(v*10+0.5)) / 10
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
