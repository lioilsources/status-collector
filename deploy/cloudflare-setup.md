# Cloudflare Tunnel — přidání hostname status.ol1n.com

Předpoklad: cloudflared tunnel na NASu již běží.

## Varianta A — Zero Trust Dashboard (doporučeno)

1. Otevřít https://one.dash.cloudflare.com → **Networks → Tunnels**
2. Kliknout na existující tunnel → **Configure → Public Hostnames**
3. **Add a public hostname:**
   - Subdomain: `status`
   - Domain: `ol1n.com`
   - Type: `HTTP`
   - URL: `127.0.0.1:80`
4. Uložit

## Varianta B — cloudflared CLI na NASu

```bash
# Přidat DNS záznam (CNAME na tunnel)
cloudflared tunnel route dns <TUNNEL_NAME> status.ol1n.com

# Přidat ingress do config.yml
# Obvykle: ~/.cloudflared/config.yml nebo /etc/cloudflared/config.yml
```

Přidat do sekce `ingress` (před fallback pravidlo):
```yaml
ingress:
  - hostname: status.ol1n.com
    service: http://127.0.0.1:80
  - service: http_status:404   # fallback — musí být poslední
```

```bash
sudo systemctl restart cloudflared
```

## Ověření

```bash
# Tunnel connected
cloudflared tunnel info <TUNNEL_NAME>

# Veřejný přístup
curl https://status.ol1n.com/api/status
```
