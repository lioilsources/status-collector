# ol1n-status

Status page pro **llm.ol1n.com** — zobrazuje hodinovou dostupnost vLLM, OpenAI-compatible a health endpointů za posledních 30 dní.

## Architektura

```
NAS (Ubuntu)
  └─ status-collector (Go binary)
       ├─ každou hodinu pinguje endpointy na llm.ol1n.com
       ├─ ukládá výsledky do SQLite (/var/lib/ol1n-status/status.db)
       └─ HTTP API na 127.0.0.1:8765
            ├─ GET /api/status          → JSON pro frontend
            └─ GET /api/history/{id}    → raw hodinové buckety

Caddy (na NAS)
  └─ status.ol1n.com
       ├─ /api/*  → reverse proxy na Go collector
       └─ /*      → static HTML frontend

Cloudflare Tunnel (stávající nastavení)
  └─ status.ol1n.com → NAS:Caddy
```

## Monitorované endpointy

| ID                  | Skupina      | URL                              |
|---------------------|-------------|----------------------------------|
| health              | Health       | /health                          |
| ping                | Health       | /ping                            |
| openai_models       | OpenAI API   | /v1/models                       |
| openai_chat         | OpenAI API   | POST /v1/chat/completions        |
| openai_completions  | OpenAI API   | POST /v1/completions             |
| openai_embeddings   | OpenAI API   | POST /v1/embeddings              |
| vllm_version        | vLLM         | /version                         |
| vllm_metrics        | vLLM         | /metrics                         |
| vllm_tokenize       | vLLM         | /tokenize                        |

## Deploy

### Prerekvizity
- Go 1.22+, gcc (pro CGO/sqlite3), systemd, Caddy

### 1. Build & deploy
```bash
chmod +x deploy/deploy.sh
./deploy/deploy.sh
```

### 2. Caddy
Přidej obsah `deploy/Caddyfile.snippet` do `/etc/caddy/Caddyfile`:
```bash
sudo nano /etc/caddy/Caddyfile
# vlož snippet, pak:
sudo caddy reload
```

### 3. Cloudflare Tunnel
Přidej public hostname v Cloudflare Zero Trust:
- **Subdomain**: status  
- **Domain**: ol1n.com  
- **Service**: http://127.0.0.1:80 (Caddy)

## Konfigurace

Flags pro systemd service (`/etc/systemd/system/ol1n-status.service`):

| Flag         | Default                          | Popis                    |
|--------------|----------------------------------|--------------------------|
| `-db`        | `/var/lib/ol1n-status/status.db` | Cesta k SQLite           |
| `-addr`      | `127.0.0.1:8765`                 | API listen adresa        |
| `-interval`  | `1h`                             | Interval kontrol         |

## Přidání vlastního endpointu

Uprav `internal/checker/checker.go`, funkci `DefaultEndpoints()`:

```go
{
    ID:    "my_endpoint",
    Name:  "Moje služba",
    Group: "Custom",
    URL:   "https://llm.ol1n.com/my/path",
},
```

Pak rebuild + deploy.

## Logy

```bash
journalctl -u ol1n-status -f
```
