# Frontend stack and supply-chain policy

**Status:** decided 2026-08-11, at the start of M3b.
**Scope:** the web interface in `web/`, and the dependency policy governing it.

This records what was chosen and why, so that a future reader does not have to
re-derive it — and so that the parts which look over-cautious are recognisable
as deliberate rather than accidental.

The sibling project `tadmor` ratified a similar policy on 2026-06-19
(`docs/frontend-stack.md` there). Much of the reasoning below is taken from it.
Where Veritix departs, the departure is argued rather than assumed.

---

## 1. Why this matters more here than usual

The usual case for hardening a front end is that a compromised dependency ships
to end users. Veritix is not public-facing — it is a program the customer runs,
on their own machine or their own cloud, bound to loopback by default.

That makes the stakes higher, not lower.

The entire proposition is that commercially sensitive data never reaches a
vendor. `internal/report` enforces that inside the process: reports omit
verbatim cell values unless asked, columns are described by derived shapes, and
`TestDefaultReportContainsNoRawValues` pins it across all four formats. But the
interface a customer actually looks at runs in a browser, and it can call
`GET /runs/{id}/findings/{fid}/rows` — the one endpoint that serves raw customer
records.

So a compromised npm package in this bundle would not be a hygiene problem. It
would sit next to exactly the data the product exists to keep in, with a network
stack underneath it. **The browser is a third place the egress guarantee has to
hold**, alongside the report and (at M4) the model.

That is the reasoning behind both the dependency policy and the CSP.

## 2. Threat model

Roughly in the order these actually happen:

1. **Malicious lifecycle scripts.** Attacker code runs at *install* time, on a
   developer's machine and on CI.
2. **Compromised maintainer or hijacked package.** A legitimate package ships
   one malicious version.
3. **Typosquatting and dependency confusion.** The wrong package is installed.
4. **Transitive bloat.** A tree of a thousand packages cannot be audited, so
   trust becomes implicit and broad.
5. **Runtime exfiltration.** A compromised bundled dependency reads what the
   page can read and sends it somewhere.

Threat 5 is the one that maps directly onto Veritix's central claim, and it is
why the CSP is treated as a tested guarantee rather than a header.

## 3. The decision

- **Build:** Vite + React + TypeScript, built to `web/dist` and embedded with
  `//go:embed`. No Node at runtime.
- **Runtime dependencies: `react` and `react-dom`, and nothing else.**
- **Package manager:** pnpm 11, pinned through corepack via `packageManager`.
- **Vendoring:** commit `pnpm-lock.yaml`; never commit `node_modules/`.
- **Hardening:** install scripts blocked, 7-day publish cooldown, frozen
  lockfile, pinned registry, exact versions.
- **Delivery:** served by the Go binary behind a strict CSP with no
  `unsafe-inline` anywhere.
- **Go modules:** not vendored; `go mod verify` and `govulncheck` in CI instead.

### 3.1 Why no other runtime dependencies

tadmor took Tailwind, shadcn/ui, Radix, ECharts and react-router, because it is
a CRUD-heavy accounting application with heavy grids and charts. Veritix's
interface is a dataset list, an upload drop zone, a run history, findings
grouped by severity, and two tables. There are no charts, no editable forms
beyond two checkboxes, and no grid mechanics.

At that size the usual velocity argument for a UI kit does not apply, so the
metric this document optimises — **distinct maintainers whose code we must trust
at runtime** — can be driven almost to zero. It is:

| | Veritix | tadmor |
|---|---|---|
| Runtime packages | 3 (`react`, `react-dom`, `scheduler`) | ~8 plus a Radix tree |
| Lockfile entries | 67, of which 45 are per-platform native binaries | several hundred |
| Real distinct packages | 22, almost all build-time | — |

Two things were hand-written rather than taken as dependencies:

- **The router** (`web/src/router.ts`, about sixty lines). tadmor reversed its
  hand-rolled-router lean and adopted `react-router-dom` on the grounds that a
  hand-rolled History-API router stays small only until you need route params,
  nested layouts, scroll restoration and dirty-form navigation guards. That
  reasoning is sound and does not apply: Veritix has five flat routes, one of
  them parameterised, and no forms to guard. If that stops being true,
  `react-router-dom` is the right thing to adopt — it was measured as the
  smallest full-featured option at 4 packages and 3 maintainers.
- **The styling** (`web/src/index.css`). The palette is lifted verbatim from
  `internal/report/templates/report.html.tmpl`, so the interface and the report
  a customer downloads from it are visibly the same product. A CSS framework
  would be a second definition of the palette as well as a dependency.

### 3.2 Hardening configuration

| Where | What | Threat |
|---|---|---|
| `package.json` | `packageManager: "pnpm@11.19.0"` — corepack fetches that exact integrity-checked binary. `engines.node`. Exact dependency versions, no `^`. | 2, 3 |
| `web/.nvmrc` | Node major pinned to 24. CI reads this file, so CI and a developer cannot drift. | — |
| `pnpm-workspace.yaml` | `minimumReleaseAge: 10080` — refuse anything published in the last 7 days. | 2 |
| `pnpm-workspace.yaml` | `onlyBuiltDependencies: []` — **no package may run an install script.** | 1 |
| `.npmrc` | `ignore-scripts=true` as well, `frozen-lockfile`, `auto-install-peers=false`, `strict-peer-dependencies=true`, `registry=` pinned to npmjs. | 1, 3 |
| `.gitignore` | `pnpm-lock.yaml` committed; `node_modules/` never. | 2, 3 |

The cooldown is the single most effective control against the commonest real
attack — a legitimate package that ships one malicious release and is yanked
within days. A fresh install refusing a version published this week is the
feature working, not a failure.

The empty install-script allow-list is worth noting: tadmor needed `esbuild`
allow-listed. Vite 8 bundles with **Rolldown** and TypeScript 7 is a native
binary, and both ship per-platform packages rather than compiling at install
time, so nothing in this build needs a lifecycle script at all. Keep the list
empty; if a future dependency breaks without its script, that is the moment to
ask whether the dependency is worth it.

### 3.3 Why `node_modules/` is not committed, though Go's `vendor/` would be

They look like the same idea and are not. `node_modules/` is hundreds of
megabytes of platform-specific native binaries — a build artifact, wrong on any
machine but the one that produced it. The npm ecosystem's actual analogue of
`go.sum` is the **lockfile**, which carries an exact version and an integrity
hash per package. That is what is committed.

## 4. The CSP as a tested guarantee

`internal/api/spa.go` serves the interface under:

```
default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:;
font-src 'self'; connect-src 'self'; form-action 'self'; object-src 'none';
base-uri 'self'; frame-ancestors 'none'
```

`connect-src 'self'` is the load-bearing one: the page may talk to the server it
came from and to nothing else, so a compromised script cannot post a finding's
rows anywhere. `base-uri 'self'` matters more than it looks — an injected
`<base>` would re-point every relative URL, routing the whole API elsewhere
without touching a single fetch.

**There is no `unsafe-inline`, for scripts or for styles.** tadmor allows it for
styles because UI libraries set inline `style` attributes, and lists tightening
it as pre-launch work. Veritix has no such dependency, so every would-be inline
style is a class in `index.css` instead and the policy is strict from the start.
An inline style attribute is a place to smuggle an exfiltrating `url()`; the
convenience is not worth reopening the hole.

`TestWebInterfaceIsServedUnderAStrictCSP` asserts the policy and fails on any
`unsafe-*`, in the same spirit as `TestDefaultReportContainsNoRawValues`. The
boundary should not be looseable without a test going red.
`web.TestBundleLoadsNothingFromAnotherOrigin` is the build-time companion: it
catches an accidental CDN reference, which would otherwise fail silently at
runtime as a page that half-works.

## 5. Serving and building

- `web/embed.go` embeds `web/dist` with `//go:embed all:dist`. A committed
  `dist/.gitkeep` keeps the package compiling before any build has run — and
  `make web` re-`touch`es it, because Vite's `emptyOutDir` wipes it.
- The SPA handler is registered on `/` and serves a built asset when one is
  named, `index.html` otherwise, so `/runs/{id}/findings/{fid}` survives a
  reload. `/api/v1/*` is matched first and is never shadowed.
- Fingerprinted assets are `immutable`; `index.html` is `no-store`, because it
  is what names the current fingerprints.
- `make build` embeds whatever is in `web/dist`; `make release` builds the
  interface first. A binary built without one serves a 503 naming the command
  that fixes it, and `serve` logs a warning at startup — the API works either
  way, which is precisely why it needs saying out loud.
- In development, `make web-dev` runs Vite and proxies `/api` to
  `veritix serve`, so every fetch is same-origin in both environments.

## 6. Go modules: measured, and deliberately not vendored

tadmor vendors its Go modules and builds hermetically. That was considered here
and rejected on measurement:

| | tadmor | Veritix |
|---|---|---|
| Modules | 5 | 41 (8 direct) |
| `vendor/` size | ~8 MB, pure source | **728 MB** |
| Largest items | — | five `libduckdb_static.a` blobs, 59–79 MB each (572 MB total) |

The stated purpose of vendoring is auditable dependency diffs. A diff of a 79 MB
prebuilt static library is not reviewable by anybody, so vendoring would buy
none of it here — while adding ~572 MB to git history permanently on every
DuckDB bump. What it would buy is durability: builds that survive an unpublish
or a proxy outage. That is business continuity, not protection from malicious
code.

Go also starts from a better position than npm: `go.sum` hashes are checked
against the **checksum transparency log**, which makes a tampered module
detectable even if the proxy is compromised, and there is no install-script
equivalent to block.

So instead, CI runs:

- `go mod verify` — every module matches its recorded hash.
- `govulncheck ./...` — known vulnerabilities, with call-graph analysis so a
  vulnerable function nobody calls does not cause noise.
- `go mod tidy` with a `git diff --exit-code`, which was already there.

Adding `govulncheck` immediately paid for itself: it reported 16 standard-library
vulnerabilities reachable from this code, fixed in Go patch releases the project
was not pinned to. `go.mod` now carries `toolchain go1.26.5` and the scan is
clean.

**The honest residual:** the largest single trust item in Veritix is not any npm
package. It is the prebuilt DuckDB static libraries shipped inside a Go module,
verified by nothing beyond a `go.sum` hash of the blob itself. That was accepted
at M1 as the price of not writing a query engine, and it remains accepted — but
it should be named rather than obscured by the care taken elsewhere.

## 7. What this does not solve

- **Build-time execution.** Blocking install scripts stops install-time code,
  but the build runs Vite and Rolldown over our config. A compromised build-time
  dependency can act during the build. The cooldown and `pnpm audit` apply to
  build dependencies too.
- **A maliciously pinned version.** If a compromised release reaches the
  lockfile, every install faithfully reproduces it. The cooldown buys a
  detection window; the audit is the backstop.
- **The cooldown is probabilistic.** It helps only if a bad release is reported
  within the window. Usually, not always.
- **Determined exfiltration.** `TestBundleLoadsNothingFromAnotherOrigin` proves
  the absence of an accidental external reference, not a deliberate one — no
  grep can do that. The CSP is the actual enforcement, and it is enforced by the
  browser rather than by us.

## 8. Deferred

- **Browser-driven end-to-end tests.** The interface has been driven over HTTP
  end to end, but not yet in a real browser. Playwright means a browser-automation
  dependency tree and a downloaded browser binary, so it belongs in an isolated
  workspace the way tadmor puts it in `e2e/` — a decision to take on its own
  merits rather than fold into M3b.
- **Tightening `img-src` off `data:`.** Vite inlines small assets as data URIs.
  There are none today; if that stays true the directive can be dropped.
