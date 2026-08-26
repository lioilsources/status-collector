// Package host samples machine-level metrics — load, CPU, memory, swap, disk.
//
// Two sources, one shape: the machine the collector runs on is read straight
// from /proc, and any other machine is scraped from node_exporter. Both emit
// the same metric names so the API and the frontend never need to care which
// kind of host they are looking at.
package host

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ol1n/status-collector/internal/storage"
)

// Disk is one mounted filesystem worth reporting.
type Disk struct {
	Mount      string  `json:"mount"`
	UsedPct    float64 `json:"used_pct"`
	TotalBytes int64   `json:"total_bytes"`
}

// Sample is one reading of a machine.
type Sample struct {
	Name       string    `json:"name"`
	SampledAt  time.Time `json:"sampled_at"`
	Reachable  bool      `json:"reachable"`
	Error      string    `json:"error,omitempty"`
	Load1      float64   `json:"load1"`
	Load5      float64   `json:"load5"`
	Load15     float64   `json:"load15"`
	CPUs       int       `json:"cpus"`
	CPUUsedPct float64   `json:"cpu_used_pct"`
	// CPUValid is false until two readings exist — utilisation is a delta, and
	// 0 would otherwise be indistinguishable from a genuinely idle machine.
	CPUValid      bool    `json:"cpu_valid"`
	MemUsedPct    float64 `json:"mem_used_pct"`
	MemTotalBytes int64   `json:"mem_total_bytes"`
	SwapUsedPct   float64 `json:"swap_used_pct"`
	Disks         []Disk  `json:"disks"`
}

// Metrics flattens a sample into rows for the metrics table.
func (s Sample) Metrics() []storage.Metric {
	if !s.Reachable {
		return nil
	}
	ms := []storage.Metric{
		{Name: "load1", Value: s.Load1},
		{Name: "load5", Value: s.Load5},
		{Name: "load15", Value: s.Load15},
		{Name: "mem_used_pct", Value: s.MemUsedPct},
	}
	if s.CPUValid {
		ms = append(ms, storage.Metric{Name: "cpu_used_pct", Value: s.CPUUsedPct})
	}
	if s.SwapUsedPct > 0 {
		ms = append(ms, storage.Metric{Name: "swap_used_pct", Value: s.SwapUsedPct})
	}
	for _, d := range s.Disks {
		ms = append(ms, storage.Metric{Name: "disk_used_pct", Label: d.Mount, Value: d.UsedPct})
	}
	return ms
}

// Sampler reads one machine.
type Sampler interface {
	Name() string
	Sample(ctx context.Context) (Sample, error)
}

// cpuCounter turns the monotonic idle/total CPU counters both sources expose
// into a utilisation percentage. The first reading only primes it.
type cpuCounter struct {
	idle, total float64
	primed      bool
}

func (c *cpuCounter) update(idle, total float64) (float64, bool) {
	prevIdle, prevTotal, primed := c.idle, c.total, c.primed
	c.idle, c.total, c.primed = idle, total, true
	if !primed {
		return 0, false
	}
	dTotal := total - prevTotal
	dIdle := idle - prevIdle
	// Counters reset when the machine reboots; treat that as a fresh prime
	// rather than reporting a nonsense spike.
	if dTotal <= 0 || dIdle < 0 {
		return 0, false
	}
	return round1(clampPct((dTotal - dIdle) / dTotal * 100)), true
}

// ── Local (/proc) ───────────────────────────────────────────────────────────

// Local reads the machine the collector runs on.
type Local struct {
	name      string
	procRoot  string
	diskPaths []string

	mu  sync.Mutex
	cpu cpuCounter
}

func NewLocal(name, procRoot string, diskPaths []string) *Local {
	if procRoot == "" {
		procRoot = "/proc"
	}
	return &Local{name: name, procRoot: procRoot, diskPaths: diskPaths}
}

func (l *Local) Name() string { return l.name }

// Available reports whether this machine exposes /proc at all. macOS does not,
// so `make build` on a laptop still works — the sampler just stays off.
func (l *Local) Available() bool {
	_, err := os.Stat(l.procRoot + "/loadavg")
	return err == nil
}

func (l *Local) Sample(ctx context.Context) (Sample, error) {
	s := Sample{Name: l.name, SampledAt: time.Now().UTC()}

	load, err := os.ReadFile(l.procRoot + "/loadavg")
	if err != nil {
		s.Error = err.Error()
		return s, err
	}
	fields := strings.Fields(string(load))
	if len(fields) < 3 {
		err := fmt.Errorf("unexpected loadavg format: %q", string(load))
		s.Error = err.Error()
		return s, err
	}
	s.Load1, _ = strconv.ParseFloat(fields[0], 64)
	s.Load5, _ = strconv.ParseFloat(fields[1], 64)
	s.Load15, _ = strconv.ParseFloat(fields[2], 64)

	if idle, total, cpus, err := l.readCPU(); err == nil {
		s.CPUs = cpus
		l.mu.Lock()
		pct, ok := l.cpu.update(idle, total)
		l.mu.Unlock()
		s.CPUUsedPct, s.CPUValid = pct, ok
	}

	if err := l.readMem(&s); err != nil {
		s.Error = err.Error()
		return s, err
	}

	for _, p := range l.diskPaths {
		used, total, err := diskUsage(p)
		if err != nil || total == 0 {
			continue
		}
		s.Disks = append(s.Disks, Disk{
			Mount:      p,
			UsedPct:    round1(clampPct(float64(used) / float64(total) * 100)),
			TotalBytes: int64(total),
		})
	}

	s.Reachable = true
	return s, nil
}

// readCPU returns cumulative idle and total jiffies from the aggregate "cpu"
// line, plus the number of per-core lines.
func (l *Local) readCPU() (idle, total float64, cpus int, err error) {
	f, err := os.Open(l.procRoot + "/stat")
	if err != nil {
		return 0, 0, 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 || !strings.HasPrefix(fields[0], "cpu") {
			continue
		}
		if fields[0] != "cpu" {
			cpus++
			continue
		}
		for i, raw := range fields[1:] {
			v, convErr := strconv.ParseFloat(raw, 64)
			if convErr != nil {
				continue
			}
			total += v
			// user nice system idle iowait irq softirq steal ...
			// Time spent waiting on IO is not time spent computing.
			if i == 3 || i == 4 {
				idle += v
			}
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, 0, err
	}
	if total == 0 {
		return 0, 0, 0, fmt.Errorf("no cpu line in %s/stat", l.procRoot)
	}
	return idle, total, cpus, nil
}

func (l *Local) readMem(s *Sample) error {
	f, err := os.Open(l.procRoot + "/meminfo")
	if err != nil {
		return err
	}
	defer f.Close()

	vals := map[string]float64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, convErr := strconv.ParseFloat(fields[0], 64)
		if convErr != nil {
			continue
		}
		vals[key] = v * 1024 // meminfo is in kB
	}
	if err := sc.Err(); err != nil {
		return err
	}

	total := vals["MemTotal"]
	if total == 0 {
		return fmt.Errorf("MemTotal missing from %s/meminfo", l.procRoot)
	}
	// MemAvailable accounts for reclaimable page cache; MemFree alone would
	// make every healthy Linux box look nearly out of memory.
	avail, ok := vals["MemAvailable"]
	if !ok {
		avail = vals["MemFree"] + vals["Buffers"] + vals["Cached"]
	}
	s.MemTotalBytes = int64(total)
	s.MemUsedPct = round1(clampPct((total - avail) / total * 100))

	if swapTotal := vals["SwapTotal"]; swapTotal > 0 {
		s.SwapUsedPct = round1(clampPct((swapTotal - vals["SwapFree"]) / swapTotal * 100))
	}
	return nil
}

// ── Remote (node_exporter) ──────────────────────────────────────────────────

// skipFilesystems are pseudo-filesystems whose "usage" means nothing.
var skipFilesystems = map[string]bool{
	"tmpfs": true, "devtmpfs": true, "overlay": true, "squashfs": true,
	"ramfs": true, "autofs": true, "iso9660": true, "fuse.gvfsd-fuse": true,
	"nsfs": true, "proc": true, "sysfs": true, "cgroup": true, "cgroup2": true,
}

// NodeExporter scrapes a remote machine's node_exporter endpoint.
type NodeExporter struct {
	name   string
	url    string
	client *http.Client

	mu  sync.Mutex
	cpu cpuCounter
}

func NewNodeExporter(name, url string) *NodeExporter {
	return &NodeExporter{
		name:   name,
		url:    url,
		client: &http.Client{Timeout: 10 * time.Second},
	}
}

func (n *NodeExporter) Name() string { return n.name }

func (n *NodeExporter) Sample(ctx context.Context) (Sample, error) {
	s := Sample{Name: n.name, SampledAt: time.Now().UTC()}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.url, nil)
	if err != nil {
		s.Error = err.Error()
		return s, err
	}
	resp, err := n.client.Do(req)
	if err != nil {
		s.Error = err.Error()
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		err := fmt.Errorf("GET %s: HTTP %d", n.url, resp.StatusCode)
		s.Error = err.Error()
		return s, err
	}

	fam, err := parseProm(resp.Body)
	if err != nil {
		s.Error = err.Error()
		return s, err
	}
	n.fill(&s, fam)
	s.Reachable = true
	return s, nil
}

func (n *NodeExporter) fill(s *Sample, fam map[string][]promSample) {
	s.Load1 = first(fam["node_load1"])
	s.Load5 = first(fam["node_load5"])
	s.Load15 = first(fam["node_load15"])

	// node_cpu_seconds_total is per core and per mode; sum both dimensions to
	// get the same idle/total pair /proc/stat's aggregate line provides.
	var idle, total float64
	cores := map[string]bool{}
	for _, sample := range fam["node_cpu_seconds_total"] {
		total += sample.value
		mode := sample.labels["mode"]
		if mode == "idle" || mode == "iowait" {
			idle += sample.value
		}
		cores[sample.labels["cpu"]] = true
	}
	s.CPUs = len(cores)
	if total > 0 {
		n.mu.Lock()
		pct, ok := n.cpu.update(idle, total)
		n.mu.Unlock()
		s.CPUUsedPct, s.CPUValid = pct, ok
	}

	memTotal := first(fam["node_memory_MemTotal_bytes"])
	memAvail := first(fam["node_memory_MemAvailable_bytes"])
	if memTotal > 0 {
		if memAvail == 0 {
			memAvail = first(fam["node_memory_MemFree_bytes"]) +
				first(fam["node_memory_Cached_bytes"]) +
				first(fam["node_memory_Buffers_bytes"])
		}
		s.MemTotalBytes = int64(memTotal)
		s.MemUsedPct = round1(clampPct((memTotal - memAvail) / memTotal * 100))
	}

	if swapTotal := first(fam["node_memory_SwapTotal_bytes"]); swapTotal > 0 {
		swapFree := first(fam["node_memory_SwapFree_bytes"])
		s.SwapUsedPct = round1(clampPct((swapTotal - swapFree) / swapTotal * 100))
	}

	sizes := map[string]float64{}
	fstypes := map[string]string{}
	for _, sample := range fam["node_filesystem_size_bytes"] {
		mp := sample.labels["mountpoint"]
		sizes[mp] = sample.value
		fstypes[mp] = sample.labels["fstype"]
	}
	for _, sample := range fam["node_filesystem_avail_bytes"] {
		mp := sample.labels["mountpoint"]
		size := sizes[mp]
		if size <= 0 || skipFilesystems[fstypes[mp]] {
			continue
		}
		s.Disks = append(s.Disks, Disk{
			Mount:      mp,
			UsedPct:    round1(clampPct((size - sample.value) / size * 100)),
			TotalBytes: int64(size),
		})
	}
	sort.Slice(s.Disks, func(i, j int) bool { return s.Disks[i].Mount < s.Disks[j].Mount })
}

// ── Prometheus text parsing ─────────────────────────────────────────────────

type promSample struct {
	labels map[string]string
	value  float64
}

// parseProm reads the Prometheus text exposition format. Only what node_exporter
// actually emits is handled — no histograms, no exemplars, no timestamps —
// which is why this is 40 lines instead of a dependency.
func parseProm(r io.Reader) (map[string][]promSample, error) {
	out := map[string][]promSample{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, rest := line, ""
		labels := map[string]string{}
		if open := strings.IndexByte(line, '{'); open >= 0 {
			close := strings.LastIndexByte(line, '}')
			if close < open {
				continue
			}
			name = line[:open]
			labels = parseLabels(line[open+1 : close])
			rest = line[close+1:]
		} else {
			idx := strings.IndexAny(line, " \t")
			if idx < 0 {
				continue
			}
			name, rest = line[:idx], line[idx:]
		}

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil {
			continue // NaN, +Inf and friends are not useful here
		}
		out[name] = append(out[name], promSample{labels: labels, value: v})
	}
	return out, sc.Err()
}

// parseLabels splits a label set, honouring quoted commas such as
// mountpoint="/mnt/a,b".
func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.TrimSpace(s[:eq])
		rest := strings.TrimSpace(s[eq+1:])
		if !strings.HasPrefix(rest, `"`) {
			break
		}
		value, remainder, ok := scanQuoted(rest)
		if !ok {
			break
		}
		out[key] = value
		s = strings.TrimPrefix(strings.TrimSpace(remainder), ",")
	}
	return out
}

func scanQuoted(s string) (value, rest string, ok bool) {
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		switch s[i] {
		case '\\':
			if i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(s[i])
				}
			}
		case '"':
			return b.String(), s[i+1:], true
		default:
			b.WriteByte(s[i])
		}
	}
	return "", "", false
}

func first(samples []promSample) float64 {
	if len(samples) == 0 {
		return 0
	}
	return samples[0].value
}

func clampPct(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }

// ── Registry ────────────────────────────────────────────────────────────────

// Registry samples every configured machine on one tick and keeps the latest
// reading of each for the API's "now" block.
type Registry struct {
	samplers []Sampler
	db       *storage.DB
	logger   *slog.Logger

	mu   sync.RWMutex
	last map[string]Sample
}

func NewRegistry(db *storage.DB, logger *slog.Logger, samplers ...Sampler) *Registry {
	return &Registry{
		samplers: samplers,
		db:       db,
		logger:   logger,
		last:     map[string]Sample{},
	}
}

func (r *Registry) Len() int { return len(r.samplers) }

// Names lists the configured hosts in declaration order, so the page does not
// reshuffle its sections between reloads.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.samplers))
	for _, s := range r.samplers {
		out = append(out, s.Name())
	}
	return out
}

// Latest returns the most recent sample per host, in declaration order. Hosts
// never sampled yet come back as unreachable placeholders rather than missing.
func (r *Registry) Latest() []Sample {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Sample, 0, len(r.samplers))
	for _, s := range r.samplers {
		if got, ok := r.last[s.Name()]; ok {
			out = append(out, got)
			continue
		}
		out = append(out, Sample{Name: s.Name(), Error: "not sampled yet"})
	}
	return out
}

// SampleAll reads every host. One unreachable machine must not stop the others,
// so failures are recorded on the sample and logged, never returned.
func (r *Registry) SampleAll(ctx context.Context) {
	for _, sampler := range r.samplers {
		sample, err := sampler.Sample(ctx)
		if err != nil {
			r.logger.Warn("host sample failed", "host", sampler.Name(), "err", err)
		}

		r.mu.Lock()
		r.last[sampler.Name()] = sample
		r.mu.Unlock()

		if metrics := sample.Metrics(); len(metrics) > 0 {
			if err := r.db.InsertMetrics(sampler.Name(), sample.SampledAt, metrics); err != nil {
				r.logger.Warn("host metrics insert failed", "host", sampler.Name(), "err", err)
			}
		}
	}
}

// ParseNodeExporters turns "spark=http://host:9100/metrics,other=..." into
// samplers.
func ParseNodeExporters(spec string) ([]Sampler, error) {
	var out []Sampler
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, url, ok := strings.Cut(part, "=")
		name, url = strings.TrimSpace(name), strings.TrimSpace(url)
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("bad -node-exporter entry %q, want name=url", part)
		}
		if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			return nil, fmt.Errorf("bad -node-exporter URL %q, want http:// or https://", url)
		}
		out = append(out, NewNodeExporter(name, url))
	}
	return out, nil
}
