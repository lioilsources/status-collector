# StatusPage — CLAUDE.md

## Overview

Status page for `llm.ol1n.com`. A Go binary (`status-collector`) runs on the NAS,
probes endpoints hourly, samples ComfyUI metrics every minute, stores everything
in SQLite and exposes a JSON API. The static frontend is hosted on **GitHub
Pages**, deliberately off the NAS — a status page served by the machine it
monitors is unreachable exactly when it is needed.

Live at: `status.ol1n.com` (GitHub Pages) · API at `status-api.ol1n.com` (NAS).

## Architecture

```
NAS (Ubuntu)
  └─ status-collector (Go binary, systemd) — three independent clocks
       ├─ -interval 1h        availability probes   → checks
       ├─ -sample-interval 1m ComfyUI gauges        → metrics
       ├─ -sample-interval 1m host metrics          → metrics
       ├─ -drain-interval 5m  ComfyUI /history      → comfy_jobs
       ├─ -snapshot-interval 5m  writes status.json + comfy.json
       └─ HTTP API on 127.0.0.1:8765
            ├─ GET /api/status        → availability + hourly buckets
            ├─ GET /api/comfy         → ComfyUI metrics
            ├─ GET /api/hosts         → CPU/RAM/load/disk per machine
            └─ GET /api/history/{id}  → raw hourly buckets

  └─ systemd timer (15m) → deploy/publish-snapshot.sh
       force-pushes the snapshot to the repo's orphan `data` branch

Caddy (NAS) → status-api.ol1n.com → :8765, via Cloudflare Tunnel
GitHub Actions → GitHub Pages → status.ol1n.com

Frontend tries the live API first, falls back to
raw.githubusercontent.com/<repo>/data/status.json, and labels stale data.
```

## Build & Deploy

```bash
make build        # → bin/status-collector
make build-linux  # → bin/status-collector-linux-{amd64,arm64}
make test         # go test ./...
make logs         # journalctl -u ol1n-status -f

./deploy/deploy.sh          # build here, then install (needs Go on this machine)
./deploy/install.sh <bin>   # install only — what the release tarball runs
```

The NAS installs from a release tarball (`.github/workflows/release.yml`), so it
needs neither a toolchain nor a checkout. Tag `v*` to publish one.

Frontend deploys on push to `main` via `.github/workflows/pages.yml`. Never push
a `gh-pages` branch — with Pages Source set to GitHub Actions that push silently
does nothing while the live site goes stale.

## Project Structure

```
cmd/status-collector/main.go   # flags, the three tickers, HTTP server
cmd/seed-demo/main.go          # dev-only: fake data + snapshot for frontend work

internal/api/api.go            # /api/status, /api/comfy, /api/history, snapshot writer
internal/checker/checker.go    # HTTP health probes; DefaultEndpoints(comfyBase)
internal/comfy/comfy.go        # ComfyUI poller: gauges + job history
internal/host/host.go          # machine metrics: /proc locally, node_exporter remotely
internal/host/disk.go          # statfs wrapper (Bsize differs across platforms)
internal/storage/storage.go    # SQLite: checks, metrics, comfy_jobs

web/index.html                 # single-file frontend, no build step
web/CNAME                      # written by the workflow, only when DNS is ready

deploy/                        # systemd units, Caddy snippet, publish script
```

## Conventions

- SQLite is `modernc.org/sqlite` — pure Go, so `CGO_ENABLED=0` builds and the
  binary cross-compiles to linux/amd64 and linux/arm64 from any machine. Do not
  reintroduce a cgo driver: the NAS deploy is a downloaded binary and has no
  toolchain.
- State: `/var/lib/ol1n-status/status.db`, snapshot in `snapshot/` beside it.
- Service user/group: `ol1n` by default, overridable with `OL1N_USER`.
  install.sh rewrites `User=`/`Group=` in the units to match, because chowning
  the data directory to one user while systemd runs the service as another
  fails at runtime with nothing obvious in the logs.
- Frontend has **no build step and no dependencies**. Keep it that way; the only
  external reference is the Google Fonts `@import`.
- `<meta name="api-base">` and `<meta name="snapshot-base">` are left empty in
  the repo and stamped by the workflow. Empty means "same origin", so the file
  works when opened locally.
- Availability is drawn as a cell grid (a handful of states); every magnitude
  over time is a line chart. Series colours live in the `--series-*` custom
  properties and were validated for the dark chart surface — do not add a hue
  without re-checking CVD separation, and keep green/red/amber reserved for
  status.
- Host metrics use one shape for both sources: the local /proc sampler and the
  node_exporter scraper emit identical metric names, so nothing downstream has
  to know which kind of machine it is looking at. `metrics.source` is the host
  name. CPU is a delta and is suppressed until two readings exist (`cpu_valid`).
- ComfyUI history reads must stay idempotent: upsert by `prompt_id`, never
  insert. ComfyUI drops its history on restart, so `comfy_jobs` is the record.
- New tables use `CREATE TABLE IF NOT EXISTS`; there is no migration version
  table, so any future `ALTER` must tolerate being re-run.
