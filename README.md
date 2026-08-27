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
       ├─ 1× za minutu   vzorkuje stroje            → SQLite (tabulka metrics)
       │                  NAS z /proc, ostatní z node_exporteru
       ├─ 1× za 5 minut  čte ComfyUI /history      → SQLite (tabulka comfy_jobs)
       ├─ 1× za 5 minut  zapisuje snapshot JSON    → /var/lib/ol1n-status/snapshot
       └─ HTTP API na 127.0.0.1:8765
            ├─ GET /api/status        → dostupnost + hodinové buckety
            ├─ GET /api/comfy         → ComfyUI metriky
            ├─ GET /api/hosts         → CPU/RAM/load/disk per stroj
            └─ GET /api/history/{id}  → raw hodinové buckety

  └─ systemd timer (15 min)
       └─ publish-snapshot.sh → force-push do větve `data`

Cloudflare Tunnel (NAS) → status-api.ol1n.com → 127.0.0.1:8765
GitHub Pages → status.ol1n.com     → web/index.html

Frontend čte živé API; když je nedostupné, spadne na
raw.githubusercontent.com/<repo>/data/status.json a ukáže poslední známý stav.
```

## Monitorované endpointy

| ID                  | Skupina    | URL                       |
|---------------------|------------|---------------------------|
| health              | Health     | /health                   |
| openai_models       | OpenAI API | /v1/models                |
| openai_chat         | OpenAI API | POST /v1/chat/completions |
| openai_completions  | OpenAI API | POST /v1/completions      |
| sonarr_ping         | Sonarr     | localhost:8989/ping       |
| radarr_ping         | Radarr     | localhost:7878/ping       |
| comfyui_stats       | ComfyUI    | $COMFY/system_stats       |
| comfyui_queue       | ComfyUI    | $COMFY/queue              |

ComfyUI endpointy vzniknou jen když je nastavený flag `-comfy`.

**Co se nemonitoruje a proč.** Tyhle cesty `llm.ol1n.com` nenabízí — proměřeno
proti živému hostu 26. 8. 2026. Probe, který nemůže nikdy zezelenat, je horší
než žádný: naučí člověka stránku ignorovat.

| cesta | výsledek | proč |
|---|---|---|
| `/ping` | 404 | host servíruje `/health`, ne `/ping` |
| `/version`, `/metrics`, `/tokenize` | 404 | vLLM-nativní routy; tenhle host vystavuje jen OpenAI-kompatibilní povrch |
| `POST /v1/embeddings` | 500 | routa existuje, ale nasazený model nemá embedding hlavu |

Kdyby je gateway začala vystavovat, přidej je zpátky s cestou, kterou opravdu
servíruje — seznam rout dá `GET /openapi.json`, ne hádání.

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

## Metriky strojů

NAS se čte přímo z `/proc` — bez agenta, bez závislostí:

| Metrika | Zdroj |
|---|---|
| load1 / load5 / load15 | `/proc/loadavg` |
| CPU % | delta `/proc/stat`, idle+iowait proti součtu |
| RAM % | `/proc/meminfo`, `MemAvailable` (ne `MemFree`) |
| swap % | `/proc/meminfo` |
| disk % per mount | `statfs`, rezervované bloky se počítají jako obsazené |

Které svazky se hlásí, řídí `HOST_DISKS` v `/etc/default/ol1n-status`:

```
HOST_DISKS="-host-disks /,/media,/pool"
```

Dva mount pointy na **stejném** filesystému se hlásí jednou, pod tou cestou,
která ho pojmenovala první — jinak by `/` a `/var/lib/ol1n-status` na jednom
disku vyrobily dva identické řádky. Cesta, která neexistuje nebo není
namontovaná, se při startu vypíše do logu jako varování, aby překlep nezmizel
potichu.

U vzdálených strojů se filesystémy pod 1 GiB ignorují — `/boot/efi` na 2 %
nikomu nic neřekne a bere řádek svazkům, na kterých záleží.

Vzdálené stroje (SPARK a cokoliv dalšího) přes **node_exporter**. Metrické názvy
jsou stejné jako u NASu, takže frontend nerozlišuje lokální a vzdálený stroj.

```bash
# na SPARKu
sudo apt install prometheus-node-exporter     # nebo binárka z github.com/prometheus/node_exporter
sudo systemctl enable --now prometheus-node-exporter
curl -s localhost:9100/metrics | head -5
```

Pak na NASu do `/etc/default/ol1n-status`:
```
NODE_EXPORTERS="-node-exporter spark=http://192.168.1.51:9100/metrics"
```
Dalších strojů může být kolik chceš: `spark=http://…,gpu2=http://…`.

> node_exporter poslouchá defaultně na všech rozhraních. Omez ho na LAN
> (`--web.listen-address=192.168.1.51:9100`) nebo firewallem — neexportuj ho
> do internetu.

**CPU je delta**, takže po startu collectoru je první vzorek bez CPU hodnoty
(v UI `–`, pole `cpu_valid: false`). Druhý vzorek už číslo má. Stejně tak se
ignoruje pokles čítačů po rebootu, aby nevznikl falešný špičkový údaj.

## Deploy

### Pořadí prvního rozjetí

Každý krok je funkční sám o sobě; stránka se s každým dalším zlepší.

**1. Collector na NAS**

Na NASu není potřeba Go, gcc ani klon tohoto repa — stačí `curl` a `tar`:

```bash
# Vyhrazený systémový účet — démon drží deploy key se zápisem do repa,
# takže nemá běžet pod tvým osobním účtem.
id ol1n || sudo useradd --system --home /var/lib/ol1n-status --shell /usr/sbin/nologin ol1n

ARCH=$(dpkg --print-architecture)          # amd64 nebo arm64
cd /tmp
curl -fsSLO "https://github.com/lioilsources/status-collector/releases/latest/download/status-collector_linux_${ARCH}.tar.gz"
tar xzf "status-collector_linux_${ARCH}.tar.gz"
cd "status-collector_linux_${ARCH}"
sudo ./install.sh
```

Chceš to pod jiným účtem? `OL1N_USER=jmeno sudo -E ./install.sh` — `install.sh`
to jméno propíše i do systemd unitů.

Tarball obsahuje binárku, `install.sh`, `publish-snapshot.sh` a systemd unity.
Aktualizace = totéž znovu.

Nový release se vydá tagem:
```bash
git tag -a v0.2.0 -m "..." && git push origin v0.2.0
# kdyby push tagu workflow nespustil:
gh workflow run release.yml -f tag=v0.2.0
```

<details>
<summary>Build ze zdrojáků (když už Go na stroji máš)</summary>

```bash
./deploy/deploy.sh          # build + install na tomhle stroji
make build-linux            # jen zkřížit binárku pro NAS odkudkoliv
```

SQLite je `modernc.org/sqlite`, tedy čisté Go — `CGO_ENABLED=0` funguje a
binárka se dá zkompilovat i z macOS pro linux/amd64 i arm64.
</details>

Vytvoř `/etc/default/ol1n-status`:
```
COMFY_FLAG="-comfy http://192.168.1.50:8188"
NODE_EXPORTERS="-node-exporter spark=http://192.168.1.51:9100/metrics"
CF_ACCESS_CLIENT_ID=<id>.access
CF_ACCESS_CLIENT_SECRET=<secret>
```
a `sudo systemctl restart ol1n-status`. Ověř: `curl -s localhost:8765/api/comfy | jq .now`

> **Service token není volitelný.** `llm.ol1n.com` je za Cloudflare Access
> (vlastní Access aplikace, AUD `9034f196…`) a odmítá neautentizované
> požadavky už na edge — 403 přijde dřív, než se dotaz dostane k originu.
> Bez tokenu hlásí status page down úplně všechno. Token vytvoř v Zero Trust
> → Access → Service Auth a autorizuj ho pro aplikaci `llm.ol1n.com`.
> ComfyUI ho nepotřebuje, protože se na něj chodí přímo přes LAN.

Ověření, že token funguje:
```bash
curl -s -o /dev/null -w "%{http_code}\n" \
  -H "CF-Access-Client-Id: $CF_ACCESS_CLIENT_ID" \
  -H "CF-Access-Client-Secret: $CF_ACCESS_CLIENT_SECRET" \
  https://llm.ol1n.com/health      # 200, ne 403
```

**2. Deploy key pro publikaci snapshotu**
```bash
sudo -u ol1n ssh-keygen -t ed25519 -N '' -f /var/lib/ol1n-status/deploy_key
sudo cat /var/lib/ol1n-status/deploy_key.pub
```
Veřejný klíč vlož do repo Settings → Deploy keys, **Allow write access**.
Pak `sudo systemctl start ol1n-status-snapshot.service` a zkontroluj, že vznikla
větev `data` se soubory `status.json`, `comfy.json` a `hosts.json`.

**3. GitHub Pages**
Settings → Pages → Source: **GitHub Actions**. Push do `main` → workflow
`Deploy status page` nasadí `web/` na `https://lioilsources.github.io/status-collector/`.
Stránka v tuhle chvíli jede ze snapshotu.

> Nepoužívej větev `gh-pages`. Když je Source nastavený na Actions, push do
> `gh-pages` tiše „uspěje" a web zůstane starý.

**4. Živé API (volitelné, ale doporučené)**
Tunel míří na kolektor přímo, žádná reverzní proxy mezi tím není:
```bash
# do ingress: v ~/.cloudflared/config.yml, PŘED catch-all http_status:404
#   - hostname: status-api.ol1n.com
#     service: http://localhost:8765
cloudflared tunnel --config ~/.cloudflared/config.yml ingress validate
cloudflared tunnel route dns <TUNNEL_NAME> status-api.ol1n.com
systemctl --user restart cloudflared-<jméno>   # nebo sudo systemctl restart cloudflared
```
Ověř `curl -i https://status-api.ol1n.com/api/status | head`: musí přijít `200`
a `access-control-allow-origin: *`. Mimo `/api/*` vrací kolektor 404.

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
| `-host-name`         | `nas`                            | Jméno tohoto stroje; prázdné = vypnuto |
| `-host-disks`        | `/,/var/lib/ol1n-status`         | Mount pointy pro disk usage; přes `HOST_DISKS` |
| `-node-exporter`     | *(prázdné)*                      | `name=url[,name=url]` vzdálených strojů |
| `-snapshot-dir`      | *(prázdné)*                      | Kam psát snapshot; prázdné = vypnuto   |
| `-snapshot-interval` | `5m`                             | Jak často přepisovat snapshot          |

Hodinový tik je pro hloubku fronty k ničemu — fronta se mění v řádu minut,
proto běží gauge sampler na vlastních hodinách.

## Vývoj

```bash
make build            # → bin/status-collector
make build-linux      # → bin/status-collector-linux-{amd64,arm64}
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
