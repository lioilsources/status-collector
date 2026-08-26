# ol1n-status

Status page pro **llm.ol1n.com** — hodinová dostupnost vLLM, OpenAI-compatible
a health endpointů za 30 dní, plus metriky ComfyUI (fronta, průchodnost, VRAM).

Frontend běží na **GitHub Pages**, ne na NASu. Status page, která leží spolu
s tím, co monitoruje, je k ničemu přesně ve chvíli, kdy ji člověk otevírá.

## Architektura

```
NAS (Ubuntu)
  └─ status-collector (Go binary, systemd)
       ├─ 1× za hodinu   pinguje endpointy         → SQLite (tabulka checks)
       ├─ 1× za minutu   vzorkuje ComfyUI gauge    → SQLite (tabulka metrics)
       ├─ 1× za 5 minut  čte ComfyUI /history      → SQLite (tabulka comfy_jobs)
       ├─ 1× za 5 minut  zapisuje snapshot JSON    → /var/lib/ol1n-status/snapshot
       └─ HTTP API na 127.0.0.1:8765
            ├─ GET /api/status        → dostupnost + hodinové buckety
            ├─ GET /api/comfy         → ComfyUI metriky
            └─ GET /api/history/{id}  → raw hodinové buckety

  └─ systemd timer (15 min)
       └─ publish-snapshot.sh → force-push do větve `data`

Caddy (NAS) → status-api.ol1n.com  → 127.0.0.1:8765     (přes Cloudflare Tunnel)
GitHub Pages → status.ol1n.com     → web/index.html

Frontend čte živé API; když je nedostupné, spadne na
raw.githubusercontent.com/<repo>/data/status.json a ukáže poslední známý stav.
```

## Monitorované endpointy

| ID                  | Skupina    | URL                       |
|---------------------|------------|---------------------------|
| health              | Health     | /health                   |
| ping                | Health     | /ping                     |
| openai_models       | OpenAI API | /v1/models                |
| openai_chat         | OpenAI API | POST /v1/chat/completions |
| openai_completions  | OpenAI API | POST /v1/completions      |
| openai_embeddings   | OpenAI API | POST /v1/embeddings       |
| vllm_version        | vLLM       | /version                  |
| vllm_metrics        | vLLM       | /metrics                  |
| vllm_tokenize       | vLLM       | /tokenize                 |
| sonarr_ping         | Sonarr     | localhost:8989/ping       |
| radarr_ping         | Radarr     | localhost:7878/ping        |
| comfyui_stats       | ComfyUI    | $COMFY/system_stats       |
| comfyui_queue       | ComfyUI    | $COMFY/queue              |

ComfyUI endpointy vzniknou jen když je nastavený flag `-comfy`.

## ComfyUI metriky

Vše ze tří prostých GETů; žádný plugin, žádná změna v ComfyUI.

| Metrika | Zdroj |
|---|---|
| hloubka fronty (running / pending) | `GET /queue`, vzorek po 60 s |
| VRAM per GPU, RAM, verze | `GET /system_stats` |
| doba běhu jobu p50/p95 | `GET /history` → `execution_success − execution_start` |
| čekání ve frontě p50/p95 | `extra_data.create_time → execution_start` |
| obrázků za den, success/error rate | `GET /history` → `outputs`, `status.status_str` |
| cache hit ratio | `execution_cached.nodes` / počet nodů v promptu |

Pokud ComfyUI `create_time` nevydává, collector odvodí čekání z toho, kdy prompt
poprvé viděl v `queue_pending` — dolní odhad, v UI označený jako odhad.

ComfyUI drží historii jen v paměti a při restartu ji ztratí, proto si collector
joby ukládá do vlastní tabulky. Čtení historie je idempotentní (upsert podle
`prompt_id`), takže opakované čtení stejného okna nic nezdvojí.

## Deploy

### Pořadí prvního rozjetí

Každý krok je funkční sám o sobě; stránka se s každým dalším zlepší.

**1. Collector na NAS**
```bash
./deploy/deploy.sh
```
Pro ComfyUI vytvoř `/etc/default/ol1n-status`:
```
COMFY_FLAG="-comfy http://192.168.1.50:8188"
```
a `sudo systemctl restart ol1n-status`. Ověř: `curl -s localhost:8765/api/comfy | jq .now`

**2. Deploy key pro publikaci snapshotu**
```bash
sudo -u ol1n ssh-keygen -t ed25519 -N '' -f /var/lib/ol1n-status/deploy_key
sudo cat /var/lib/ol1n-status/deploy_key.pub
```
Veřejný klíč vlož do repo Settings → Deploy keys, **Allow write access**.
Pak `sudo systemctl start ol1n-status-snapshot.service` a zkontroluj, že vznikla
větev `data` se soubory `status.json` a `comfy.json`.

**3. GitHub Pages**
Settings → Pages → Source: **GitHub Actions**. Push do `main` → workflow
`Deploy status page` nasadí `web/` na `https://lioilsources.github.io/status-collector/`.
Stránka v tuhle chvíli jede ze snapshotu.

> Nepoužívej větev `gh-pages`. Když je Source nastavený na Actions, push do
> `gh-pages` tiše „uspěje" a web zůstane starý.

**4. Živé API (volitelné, ale doporučené)**
Přidej v Cloudflare Tunnelu public hostname `status-api.ol1n.com` →
`http://127.0.0.1:80` a vlož `deploy/Caddyfile.snippet` do `/etc/caddy/Caddyfile`.
Ověř `curl -i https://status-api.ol1n.com/api/status | head`.

**5. Vlastní doména (až úplně nakonec)**
V Cloudflare DNS přidej `CNAME status → lioilsources.github.io` jako
**DNS only (šedý mráček)** — s proxy si GitHub nedokáže doménu ověřit a
certifikát se nevydá. Pak nastav v `.github/workflows/pages.yml`
`CUSTOM_DOMAIN: status.ol1n.com` a pushni. Po vydání certifikátu a zapnutí
*Enforce HTTPS* jde volitelně přepnout na proxied + SSL Full (strict).

### Konfigurace collectoru

| Flag                 | Default                          | Popis                                  |
|----------------------|----------------------------------|----------------------------------------|
| `-db`                | `/var/lib/ol1n-status/status.db` | Cesta k SQLite                         |
| `-addr`              | `:8765`                          | API listen adresa                      |
| `-interval`          | `1h`                             | Interval kontrol dostupnosti           |
| `-comfy`             | *(prázdné)*                      | ComfyUI base URL; prázdné = vypnuto    |
| `-sample-interval`   | `60s`                            | Vzorkování ComfyUI gauge               |
| `-drain-interval`    | `5m`                             | Čtení ComfyUI historie                 |
| `-snapshot-dir`      | *(prázdné)*                      | Kam psát snapshot; prázdné = vypnuto   |
| `-snapshot-interval` | `5m`                             | Jak často přepisovat snapshot          |

Hodinový tik je pro hloubku fronty k ničemu — fronta se mění v řádu minut,
proto běží gauge sampler na vlastních hodinách.

## Vývoj

```bash
make build            # → bin/status-collector
make test             # go test ./...
make logs             # journalctl -u ol1n-status -f
```

Frontend bez NASu — `cmd/seed-demo` naplní throwaway databázi věrohodnými daty
a vygeneruje snapshot, který stačí naservírovat vedle `web/index.html`:

```bash
go run ./cmd/seed-demo -db /tmp/demo.db -out /tmp/demo-snapshot
```

Prázdné `<meta name="api-base">` znamená „stejný origin", takže `web/index.html`
funguje i otevřený lokálně. Produkční hodnoty do meta tagů doplní až workflow.

## Přidání endpointu

`internal/checker/checker.go`, funkce `DefaultEndpoints`:

```go
{
    ID:    "my_endpoint",
    Name:  "Moje služba",
    Group: "Custom",
    URL:   base + "/my/path",
},
```

Nová hodnota v `Group` si ve frontendu vyrobí vlastní sekci sama.
