# Cloudflare — DNS a Tunnel

Po přesunu frontendu na GitHub Pages má `ol1n.com` dva různé záznamy:

| Hostname              | Kam                | Typ záznamu                              |
|-----------------------|--------------------|------------------------------------------|
| `status.ol1n.com`     | GitHub Pages       | `CNAME → lioilsources.github.io`, **DNS only** |
| `status-api.ol1n.com` | NAS Caddy → :8765  | vytvoří Cloudflare Tunnel automaticky    |

## 1. API hostname v tunelu

Předpoklad: `cloudflared` tunnel na NASu už běží.

**Zero Trust Dashboard:** https://one.dash.cloudflare.com → **Networks → Tunnels**
→ existující tunnel → **Configure → Public Hostnames → Add a public hostname**

- Subdomain: `status-api`
- Domain: `ol1n.com`
- Type: `HTTP`
- URL: `http://127.0.0.1:80` (Caddy)

**Nebo přes CLI:**
```bash
cloudflared tunnel route dns <TUNNEL_NAME> status-api.ol1n.com
```
a do `config.yml` mezi `ingress:`:
```yaml
  - hostname: status-api.ol1n.com
    service: http://127.0.0.1:80
```

Ověření:
```bash
curl -i https://status-api.ol1n.com/api/status | head -20
```
Musí přijít `200` a hlavička `access-control-allow-origin: *` — bez ní si
stránka na Pages data nepřečte.

## 2. DNS pro GitHub Pages

V Cloudflare DNS pro zónu `ol1n.com`:

```
CNAME   status   lioilsources.github.io   (Proxy status: DNS only — šedý mráček)
```

**Šedý mráček je důležitý.** S proxy si GitHub nedokáže doménu ověřit,
certifikát se nevydá a *Enforce HTTPS* zůstane šedé. Až certifikát naskočí
(repo Settings → Pages), jde volitelně přepnout na proxied + SSL **Full (strict)**.

Teprve potom nastav v `.github/workflows/pages.yml`:
```yaml
CUSTOM_DOMAIN: status.ol1n.com
```
Dřív ne — CNAME soubor v artefaktu přesměruje `lioilsources.github.io/status-collector/`
na doménu, která ještě neexistuje, a status page je nedostupná úplně.

## 3. Co se smazalo

Starý public hostname `status.ol1n.com → http://127.0.0.1:80` v tunelu už není
potřeba a musí zmizet, jinak si bude s DNS záznamem pro Pages konkurovat.
