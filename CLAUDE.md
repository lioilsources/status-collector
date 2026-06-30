# StatusPage — CLAUDE.md

## Overview

Status page for `llm.ol1n.com`. A Go binary (`status-collector`) runs on the NAS, pings LLM endpoints hourly, stores results in SQLite, and exposes a JSON API. A static HTML frontend fetches the API and renders 30-day uptime grid.

Live at: `status.ol1n.com`

## Architecture

```
NAS (Ubuntu)
  └─ status-collector (Go binary, systemd)
       ├─ hourly: ping all endpoints → SQLite
       └─ HTTP API on 127.0.0.1:8765
            ├─ GET /api/status       → JSON (frontend consumes this)
            └─ GET /api/history/{id} → raw hourly buckets

Caddy (NAS)
  └─ status.ol1n.com
       ├─ /api/* → proxy to :8765
       └─ /*     → static web/ directory

Cloudflare Tunnel → NAS Caddy
```

## Build & Deploy

```bash
# Build
make build          # → bin/status-collector

# Deploy to NAS (install + restart systemd service)
make install        # sudo install + systemctl restart ol1n-status

# Logs
make logs           # journalctl -u ol1n-status -f

# Status
make status
```

## Project Structure

```
cmd/status-collector/
  main.go           # HTTP server + hourly tick
  api.go            # /api/status, /api/history/{id} handlers

internal/
  api/              # HTTP handler helpers
  checker/          # HTTP health probes for each endpoint
  storage/          # SQLite read/write (hourly buckets)

web/
  index.html        # Static frontend (IBM Plex Mono, dark theme)

deploy/
  ol1n-status.service   # systemd unit
  Caddyfile.snippet     # Caddy reverse proxy config
  deploy.sh
```

## Monitored Endpoints

Defined in collector config. Key ones:
- `GET /health` — vLLM health
- `GET /ping` — gateway ping
- `GET /v1/models` — OpenAI-compatible model list

## Conventions

- CGO enabled (SQLite via `modernc.org/sqlite` or cgo)
- Binary must be cross-compiled for NAS (Linux/amd64 or arm64)
- State file: `/var/lib/ol1n-status/status.db`
- Service user: check systemd unit for user/group
