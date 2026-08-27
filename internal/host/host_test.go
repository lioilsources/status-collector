package host

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ol1n/status-collector/internal/storage"
)

// writeProc lays out a fake /proc. The CPU line is parameterised so a second
// sample can advance the counters.
func writeProc(t *testing.T, dir string, cpuLine string) {
	t.Helper()
	files := map[string]string{
		"loadavg": "1.25 0.80 0.55 2/1234 5678\n",
		"stat": cpuLine + `
cpu0 100 0 50 800 10 0 0 0 0 0
cpu1 100 0 50 800 10 0 0 0 0 0
intr 12345
ctxt 67890
`,
		"meminfo": `MemTotal:       16000000 kB
MemFree:          800000 kB
MemAvailable:    6000000 kB
Buffers:          200000 kB
Cached:          4000000 kB
SwapTotal:       4000000 kB
SwapFree:        3000000 kB
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalSample(t *testing.T) {
	dir := t.TempDir()
	// user nice system idle iowait irq softirq steal
	writeProc(t, dir, "cpu  200 0 100 1600 20 0 0 0 0 0")

	l := NewLocal("nas", dir, []string{dir})
	if !l.Available() {
		t.Fatal("Available() should be true when /proc/loadavg exists")
	}

	s, err := l.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if !s.Reachable {
		t.Fatalf("not reachable: %s", s.Error)
	}
	if s.Load1 != 1.25 || s.Load5 != 0.80 || s.Load15 != 0.55 {
		t.Errorf("load = %v/%v/%v; want 1.25/0.8/0.55", s.Load1, s.Load5, s.Load15)
	}
	if s.CPUs != 2 {
		t.Errorf("cpus = %d; want 2", s.CPUs)
	}
	// MemAvailable is 6000000 of 16000000 kB → 62.5% used.
	if s.MemUsedPct != 62.5 {
		t.Errorf("mem used = %v; want 62.5", s.MemUsedPct)
	}
	if s.MemTotalBytes != 16000000*1024 {
		t.Errorf("mem total = %d", s.MemTotalBytes)
	}
	// Swap 1000000 of 4000000 kB → 25%.
	if s.SwapUsedPct != 25 {
		t.Errorf("swap used = %v; want 25", s.SwapUsedPct)
	}
	// The very first sample cannot know CPU utilisation — it is a delta.
	if s.CPUValid {
		t.Errorf("first sample claims a valid cpu reading (%v)", s.CPUUsedPct)
	}
	if len(s.Disks) != 1 || s.Disks[0].TotalBytes <= 0 {
		t.Errorf("disks = %+v; want one real filesystem", s.Disks)
	}
}

func TestLocalCPUIsADelta(t *testing.T) {
	dir := t.TempDir()
	writeProc(t, dir, "cpu  200 0 100 1600 20 0 0 0 0 0") // total 1920, idle 1620
	l := NewLocal("nas", dir, nil)

	if _, err := l.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Advance by 100 busy and 100 idle jiffies → exactly 50% utilisation.
	writeProc(t, dir, "cpu  300 0 100 1700 20 0 0 0 0 0") // total 2120, idle 1720
	s, err := l.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUUsedPct != 50 {
		t.Errorf("cpu = %v; want 50", s.CPUUsedPct)
	}
}

func TestLocalCPUSurvivesCounterReset(t *testing.T) {
	dir := t.TempDir()
	writeProc(t, dir, "cpu  2000 0 1000 16000 200 0 0 0 0 0")
	l := NewLocal("nas", dir, nil)
	if _, err := l.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A reboot rewinds the counters; that must not produce a bogus spike.
	writeProc(t, dir, "cpu  10 0 5 80 1 0 0 0 0 0")
	s, err := l.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUValid {
		t.Errorf("cpu after a counter reset should be invalid, got %v", s.CPUUsedPct)
	}
}

func TestLocalUnavailableWithoutProc(t *testing.T) {
	l := NewLocal("nas", filepath.Join(t.TempDir(), "nope"), nil)
	if l.Available() {
		t.Error("Available() should be false without /proc")
	}
}

const nodeExporterFixture = `# HELP node_load1 1m load average.
# TYPE node_load1 gauge
node_load1 3.5
node_load5 2.25
node_load15 1.1
node_cpu_seconds_total{cpu="0",mode="idle"} 800
node_cpu_seconds_total{cpu="0",mode="user"} 150
node_cpu_seconds_total{cpu="0",mode="system"} 50
node_cpu_seconds_total{cpu="1",mode="idle"} 800
node_cpu_seconds_total{cpu="1",mode="user"} 150
node_cpu_seconds_total{cpu="1",mode="system"} 50
node_memory_MemTotal_bytes 1.34217728e+11
node_memory_MemAvailable_bytes 3.3554432e+10
node_memory_SwapTotal_bytes 8.589934592e+09
node_memory_SwapFree_bytes 8.589934592e+09
node_filesystem_size_bytes{device="/dev/nvme0n1",fstype="ext4",mountpoint="/"} 4e12
node_filesystem_avail_bytes{device="/dev/nvme0n1",fstype="ext4",mountpoint="/"} 1e12
node_filesystem_size_bytes{device="/dev/nvme0n1p1",fstype="vfat",mountpoint="/boot/efi"} 5.36870912e+08
node_filesystem_avail_bytes{device="/dev/nvme0n1p1",fstype="vfat",mountpoint="/boot/efi"} 5.2e+08
node_filesystem_size_bytes{device="tmpfs",fstype="tmpfs",mountpoint="/run"} 6.7e+10
node_filesystem_avail_bytes{device="tmpfs",fstype="tmpfs",mountpoint="/run"} 6.6e+10
node_scrape_collector_success{collector="cpu"} 1
`

func serveFixture(t *testing.T, body *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, *body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNodeExporterSample(t *testing.T) {
	body := nodeExporterFixture
	srv := serveFixture(t, &body)

	n := NewNodeExporter("spark", srv.URL+"/metrics")
	s, err := n.Sample(context.Background())
	if err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if !s.Reachable {
		t.Fatalf("not reachable: %s", s.Error)
	}
	if s.Load1 != 3.5 || s.Load5 != 2.25 || s.Load15 != 1.1 {
		t.Errorf("load = %v/%v/%v", s.Load1, s.Load5, s.Load15)
	}
	if s.CPUs != 2 {
		t.Errorf("cpus = %d; want 2", s.CPUs)
	}
	// 128 GiB total, 32 GiB available → 75% used.
	if s.MemUsedPct != 75 {
		t.Errorf("mem = %v; want 75", s.MemUsedPct)
	}
	if s.SwapUsedPct != 0 {
		t.Errorf("swap = %v; want 0 (nothing swapped)", s.SwapUsedPct)
	}
	// tmpfs is a pseudo-filesystem and /boot/efi is a 512 MB firmware
	// partition; only the real root is worth a line on the page.
	if len(s.Disks) != 1 {
		t.Fatalf("disks = %+v; want only the ext4 root", s.Disks)
	}
	if s.Disks[0].Mount != "/" || s.Disks[0].UsedPct != 75 {
		t.Errorf("disk = %+v; want / at 75%%", s.Disks[0])
	}
}

func TestNodeExporterCPUIsADelta(t *testing.T) {
	body := nodeExporterFixture
	srv := serveFixture(t, &body)
	n := NewNodeExporter("spark", srv.URL+"/metrics")

	if _, err := n.Sample(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Advance each core by 100 idle and 100 busy seconds → 50%.
	body = strings.NewReplacer(
		`node_cpu_seconds_total{cpu="0",mode="idle"} 800`, `node_cpu_seconds_total{cpu="0",mode="idle"} 900`,
		`node_cpu_seconds_total{cpu="1",mode="idle"} 800`, `node_cpu_seconds_total{cpu="1",mode="idle"} 900`,
		`node_cpu_seconds_total{cpu="0",mode="user"} 150`, `node_cpu_seconds_total{cpu="0",mode="user"} 250`,
		`node_cpu_seconds_total{cpu="1",mode="user"} 150`, `node_cpu_seconds_total{cpu="1",mode="user"} 250`,
	).Replace(nodeExporterFixture)

	s, err := n.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.CPUUsedPct != 50 {
		t.Errorf("cpu = %v; want 50", s.CPUUsedPct)
	}
}

func TestNodeExporterUnreachable(t *testing.T) {
	n := NewNodeExporter("spark", "http://127.0.0.1:1/metrics")
	s, err := n.Sample(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if s.Reachable || s.Error == "" {
		t.Errorf("sample should record the failure: %+v", s)
	}
}

func TestParseLabelsHandlesQuotedCommas(t *testing.T) {
	fam, err := parseProm(strings.NewReader(
		`node_filesystem_size_bytes{device="/dev/sda",fstype="ext4",mountpoint="/mnt/a,b"} 42` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := fam["node_filesystem_size_bytes"]
	if len(got) != 1 {
		t.Fatalf("samples = %d", len(got))
	}
	if got[0].labels["mountpoint"] != "/mnt/a,b" {
		t.Errorf("mountpoint = %q; want /mnt/a,b", got[0].labels["mountpoint"])
	}
	if got[0].value != 42 {
		t.Errorf("value = %v; want 42", got[0].value)
	}
}

func TestParsePromSkipsCommentsAndNaN(t *testing.T) {
	fam, err := parseProm(strings.NewReader(
		"# HELP x help\n# TYPE x gauge\nx 1\ny NaN\nz +Inf\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(fam["x"]) != 1 || fam["x"][0].value != 1 {
		t.Errorf("x = %+v", fam["x"])
	}
	// NaN/Inf parse as floats in Go but are useless as a gauge reading.
	for _, name := range []string{"y", "z"} {
		if len(fam[name]) == 0 {
			continue
		}
		if v := fam[name][0].value; v == v && v <= 1e308 {
			t.Errorf("%s unexpectedly usable: %v", name, v)
		}
	}
}

func TestMetricsOmitsCPUUntilPrimed(t *testing.T) {
	s := Sample{Reachable: true, Load1: 1, MemUsedPct: 50, CPUUsedPct: 0}
	if got := s.Metrics(); containsName(got, "cpu_used_pct") {
		t.Error("cpu_used_pct must be omitted before the counter is primed")
	}
	s.CPUValid = true
	if got := s.Metrics(); !containsName(got, "cpu_used_pct") {
		t.Error("cpu_used_pct must be present once primed")
	}
	if s2 := (Sample{Reachable: false, CPUValid: true}); len(s2.Metrics()) != 0 {
		t.Error("an unreachable host must not write metrics")
	}
}

func containsName(ms []storage.Metric, name string) bool {
	for _, m := range ms {
		if m.Name == name {
			return true
		}
	}
	return false
}

func TestParseNodeExporters(t *testing.T) {
	got, err := ParseNodeExporters("spark=http://192.168.1.51:9100/metrics, gpu=http://gpu:9100/metrics")
	if err != nil {
		t.Fatalf("ParseNodeExporters: %v", err)
	}
	if len(got) != 2 || got[0].Name() != "spark" || got[1].Name() != "gpu" {
		t.Fatalf("got %d samplers: %+v", len(got), got)
	}

	if _, err := ParseNodeExporters(""); err != nil {
		t.Errorf("empty spec should be a no-op, got %v", err)
	}
	for _, bad := range []string{"spark", "=http://x", "spark=", "spark=ftp://x"} {
		if _, err := ParseNodeExporters(bad); err == nil {
			t.Errorf("%q should have been rejected", bad)
		}
	}
}

func TestLocalReportsEachFilesystemOnce(t *testing.T) {
	dir := t.TempDir()
	writeProc(t, dir, "cpu  200 0 100 1600 20 0 0 0 0 0")
	sub := filepath.Join(dir, "nested")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	// Both paths live on the same filesystem — exactly the shape of
	// "/" plus "/var/lib/ol1n-status" on a box with one volume.
	l := NewLocal("nas", dir, []string{dir, sub})
	s, err := l.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Disks) != 1 {
		t.Errorf("disks = %+v; want one entry, not the same volume twice", s.Disks)
	}
	if len(s.Disks) == 1 && s.Disks[0].Mount != dir {
		t.Errorf("mount = %q; want the first path that named the volume (%q)", s.Disks[0].Mount, dir)
	}
}

func TestMissingDisksAreReported(t *testing.T) {
	dir := t.TempDir()
	writeProc(t, dir, "cpu  200 0 100 1600 20 0 0 0 0 0")
	ghost := filepath.Join(dir, "not-mounted")

	l := NewLocal("nas", dir, []string{dir, ghost})
	missing := l.MissingDisks()
	if len(missing) != 1 || missing[0] != ghost {
		t.Errorf("MissingDisks() = %v; want [%s]", missing, ghost)
	}

	// It must not break the sample, just be absent from it.
	s, err := l.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Disks) != 1 {
		t.Errorf("disks = %+v; want only the readable path", s.Disks)
	}
}
