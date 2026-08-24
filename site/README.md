# The product page

`veritix.belunaro.com` — the page somebody lands on before they have downloaded
anything. It is static: `index.html`, `style.css`, and four PNGs — the two
screenshots, each captured in both themes.

## What it is, and what it is not

**It is a marketing page, and nothing else runs on that host.** No Veritix
process, no port, no database, no customer data. That is worth stating rather
than assuming, because the whole proposition of the product is that data does
not go to a vendor, and a vendor-operated box in the same brand is exactly the
thing a careful buyer will ask about. The answer has to be "there is nothing
there but HTML".

**No build step, no JavaScript, no third-party asset.** `docs/frontend-stack.md`
argues that the shipped interface has three runtime dependencies because it sits
next to the data this product exists to keep in. A page that sits next to
nothing has even less excuse: a font from a CDN or an analytics snippet would
be a third party executing on the domain that also asks people to trust the
binary it links to. The favicon is an inline `data:` SVG for the same reason.

**The palette is lifted from `web/src/index.css`**, which lifted it from the
report template. The page, the interface and the downloaded report are one
product; three palettes would drift apart within a milestone.

## The screenshots

`report.png` and `evidence.png` are real — a genuine `veritix serve` audit of
`testdata/dirty-retail`, captured with Playwright at a device scale factor of 2.
They are not mockups, and they must not become mockups: a product page showing
an interface that does not exist is the same defect as a report that quotes a
number nothing measured.

Each has a `-dark` twin, served by a `<picture>` element on
`prefers-color-scheme`. The interface follows the system theme, so a page that
did not would show a white slab to every dark-mode reader — and dimming the
light one with CSS would have misrepresented the product to avoid capturing
it.

To regenerate them after a UI change, run a build and an audit, then screenshot:

```sh
make release
mkdir -p /tmp/vx-site && VERITIX_DATA_DIR=/tmp/vx-site ./bin/veritix serve --addr 127.0.0.1:8099 &
DS=$(curl -s -XPOST localhost:8099/api/v1/datasets -H 'Content-Type: application/json' \
      -d "{\"path\":\"$PWD/testdata/dirty-retail\"}" | jq -r .id)
RUN=$(curl -s -XPOST localhost:8099/api/v1/runs -H 'Content-Type: application/json' \
      -d "{\"dataset_id\":\"$DS\"}" | jq -r .id)
```

Then, from `e2e/` (which is where Playwright lives), with the run id above:

- the report screen at 1400×860, device scale factor 2 → `report.png`
- an element screenshot of `article.finding.target`, having navigated to
  `/runs/<run>/findings/<finding>` → `evidence.png`

Take the shot from a **clean tagged build**, not a development one: the version
string is visible in the header of `report.png`, and `v0.1.1` reads as a
product where `5f30b5c-dirty` reads as somebody's laptop.

**Check the font before believing the screenshot.** The interface asks for
`-apple-system, …, "Segoe UI", …, sans-serif`. A machine with the Liberation
fonts installed but no fontconfig — which is this development box, and is the
default in a lot of containers — resolves none of those names and Chromium
falls back to a *monospace* face. The page still lays out, so nothing looks
broken; the product simply renders as a terminal tool in every shot, which is
the opposite of what the interface exists to say. `fc-match Helvetica` is the
check. Where fontconfig is missing, point `FONTCONFIG_FILE` at a minimal
config aliasing the stack to Liberation Sans before running Playwright.

## Deploying it

The box is the belunaro VPS. The canonical description of that server lives in
the tadmor repo at `docs/belunaro-app-deployment.md`; what follows is the
static-site subset, which is all this needs.

- OVH VPS, Debian 13, `57.129.138.32`, SSH alias `vps` (user `debian`).
- `*.belunaro.com` is a wildcard already pointing at the box, so
  **no DNS work is needed** for this subdomain.
- ufw is default-deny with only 22, 80 and 443 open. A static site needs no
  port. **Never open one.**

### First time

Everything in this section needs `sudo` and is done **once**, by hand, from a
machine that already has a login on the box. Everything after it does not need
`sudo` at all — that is the point of the `chown`.

Authorize whichever machine will publish the page. Per-machine keys, never a
copy of one key: a key that exists in one place is revoked by deleting one line
here, and a copied one is not.

```sh
ssh vps 'cat >> ~/.ssh/authorized_keys' < path/to/that-machine/veritix-deploy.pub
```

Create the document root and hand it to the deploy account, so publishing is an
unprivileged write to one directory of static files rather than something that
needs root on a box that also serves another app:

```sh
ssh vps 'sudo mkdir -p /srv/www/veritix && sudo chown debian:debian /srv/www/veritix'
```

Append the contents of `veritix.Caddyfile` to `/etc/caddy/Caddyfile`, then —
**always in this order**, since a bad Caddyfile takes down every site sharing
the machine:

```sh
ssh vps 'sudo caddy validate --config /etc/caddy/Caddyfile && sudo systemctl reload caddy'
```

Caddy obtains the Let's Encrypt certificate on the first request. Then publish
and verify:

```sh
make site-deploy
curl -fsSI https://veritix.belunaro.com/ | head -1
```

### Afterwards

`make site-deploy` is the whole of it — a tar over ssh into a directory the
deploy account owns. No `sudo`, and nothing to restart: Caddy serves whatever
is on disk.

**What the deploy key can do, stated rather than assumed.** It is a login to
the `debian` account, which is in `sudo`. Nothing in the deploy path uses that,
but the account has it, so the key is worth the same care as any other login to
the box. If you want it genuinely confined to this one job, the two ways are a
`command=` restriction in `authorized_keys` pinning it to the extract, or a
separate unprivileged user owning `/srv/www/veritix`. Neither is done here.

## Rules for a shared box

Several small apps share this machine. Touch only `/srv/www/veritix` and the
`veritix.belunaro.com` vhost block. Never edit another app's vhost, restart
another app's service, change `sshd`, or open a firewall port as part of
deploying this page.
