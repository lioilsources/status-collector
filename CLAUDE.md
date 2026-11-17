# status-collector

## Účel
Status page pro `llm.ol1n.com`. Go binárka `status-collector` běží na NASu, každou
hodinu probuje endpointy, každou minutu sbírá metriky ComfyUI a strojů, ukládá je
do SQLite a vystavuje JSON API. Statický frontend je na **GitHub Pages**, záměrně
mimo NAS: status page na stroji, který hlídá, je nedostupná právě když je potřeba.

Živě: `status.ol1n.com` (Pages) · API `status-api.ol1n.com` (NAS přes Cloudflare Tunnel).

## Build a test
```bash
make build        # → bin/status-collector
make build-linux  # → bin/status-collector-linux-{amd64,arm64}
make test         # go test ./...  (gate před pushem)
make logs         # journalctl -u ol1n-status -f

./deploy/deploy.sh          # build here, then install (needs Go on this machine)
./deploy/install.sh <bin>   # install only — what the release tarball runs
```
NAS instaluje z release tarballu (tag `v*`), nemá toolchain ani checkout.

## Mapa
- `cmd/status-collector/` — flagy, tickery, HTTP server; `cmd/seed-demo/` — falešná data pro frontend.
- `internal/api|checker|comfy|host|storage/` — API a snapshot, probe, ComfyUI poller, metriky strojů, SQLite.
- `web/index.html` — jednosouborový frontend bez build kroku.
- `deploy/` — systemd unity, Cloudflare poznámky, publish skript; `deploy/spark/` — GPU textfile pro node_exporter.

## Invarianty
- Frontend deployuje jen `.github/workflows/pages.yml` při pushi do `main`.
  Nikdy nepushuj větev `gh-pages`: při Pages Source = GitHub Actions push tiše
  nic neudělá a živý web zastará.
- Žádný cgo SQLite driver: NAS dostává staženou binárku cross-kompilovanou
  pro linux/amd64 i arm64.
- Frontend **bez build kroku a bez závislostí**; jediná externí reference je
  Google Fonts `@import`.
- Čtení ComfyUI history musí zůstat idempotentní: upsert podle `prompt_id`,
  nikdy insert. ComfyUI po restartu historii zahodí, `comfy_jobs` je záznam.
- Nové tabulky přes `CREATE TABLE IF NOT EXISTS`; tabulka verzí migrací není,
  takže každý budoucí `ALTER` musí snést opakované spuštění.
- Host metriky mají jeden tvar pro /proc i node_exporter; teplota `temp_c`
  s labelem `zone`/`gpu`/`nvme`. Detaily v `docs/architecture.md`.
- Grafy: dostupnost jako mřížka buněk, každá veličina v čase jako line chart;
  zelená/červená/oranžová jen pro stav.

## Kam dál
- `docs/architecture.md` — diagram běhu, struktura souborů, deploy, model metrik, frontend.
- `README.md` — instalace na NAS a SPARK, konfigurace v `/etc/default/ol1n-status`.
- `deploy/cloudflare-setup.md` — tunel a Access.
