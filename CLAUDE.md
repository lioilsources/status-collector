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
       ├─ -drain-interval 5m  ComfyUI /history      → comfy_jobs
       ├─ -snapshot-interval 5m  writes status.json + comfy.json
       └─ HTTP API on 127.0.0.1:8765
            ├─ GET /api/status        → availability + hourly buckets
            ├─ GET /api/comfy         → ComfyUI metrics
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
make build      # → bin/status-collector
make test       # go test ./...
make install    # sudo install + systemctl restart ol1n-status
make logs       # journalctl -u ol1n-status -f

./deploy/deploy.sh   # full NAS install: binary, publish script, units, timer
```

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
internal/storage/storage.go    # SQLite: checks, metrics, comfy_jobs

web/index.html                 # single-file frontend, no build step
web/CNAME                      # written by the workflow, only when DNS is ready

deploy/                        # systemd units, Caddy snippet, publish script
```

## Conventions

- CGO enabled — SQLite via `github.com/mattn/go-sqlite3`. No cross-compile.
- State: `/var/lib/ol1n-status/status.db`, snapshot in `snapshot/` beside it.
- Service user/group: `ol1n`.
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
- ComfyUI history reads must stay idempotent: upsert by `prompt_id`, never
  insert. ComfyUI drops its history on restart, so `comfy_jobs` is the record.
- New tables use `CREATE TABLE IF NOT EXISTS`; there is no migration version
  table, so any future `ALTER` must tolerate being re-run.
