# Browser tests

Playwright tests that drive the real web interface against the Go binary serving
the embedded build — the thing that ships, not the Vite dev server.

This is a **separate pnpm workspace with its own lockfile**, so that Playwright
and its browser download never enter the shipped interface's dependency tree.
The reasoning, and the hardening that applies here, is in
[`../docs/frontend-stack.md`](../docs/frontend-stack.md) §8.

## Running them

One-time, and it needs root — the headless browser needs system packages that
are not installed by default:

```sh
sudo apt-get install -y libasound2t64 libatk1.0-0t64 libatk-bridge2.0-0t64 \
  libatspi2.0-0t64 libgbm1 libxcomposite1 libxdamage1 libxfixes3 libxrandr2 \
  fonts-liberation
```

The nine libraries are what the browser links against. **`fonts-liberation` is
just as necessary**: a machine with no fonts runs Chromium happily and renders
every page with no glyphs, which looks like a CSS bug rather than a missing
package — right layout, right colors, invisible text.

`playwright install-deps` covers all of it but pulls several hundred packages,
including Xvfb and soundfonts, that a headless run never uses.

Then, from the repository root:

```sh
make e2e          # install, build, serve on a temp data dir, test, tear down
make e2e-test     # against a server already running, for a quicker loop
```

`make e2e-install` fetches the dependencies from the frozen lockfile and then
downloads Chromium as an explicit step. That download is deliberately *not* an
install script: lifecycle scripts are blocked repo-wide, and a package fetching
binaries over the network at install time is exactly what that rule is for.

## What they check

The part no Go test can reach:

- the bundle loads and runs under the strict CSP,
- an upload of a folder becomes a dataset, named after the folder,
- SSE progress drives a re-render — findings appear with no reload,
- a finding expands to its evidence query and puts itself in the address bar,
- the offending rows stay hidden until asked for, and can be hidden again,
- a deep link to a finding survives being pasted into a fresh tab,
- the downloaded HTML report references nothing external,
- the page issues no cross-origin request at all.

`internal/api/spa_test.go` covers how the interface is *served* — the CSP header,
the route fallback, caching, the API not being shadowed — against a stub
filesystem, so `go test` never depends on a front-end build having been run.
These are the complement: what happens once a browser executes it.

## Environment

| | |
|---|---|
| `BASE_URL` | what the tests target. Default `http://localhost:8080`, the Go binary. Set to `http://localhost:5173` to drive `make web-dev` instead. |

Tests run serially against one server and one data directory: the sidebar lists
every dataset that exists, so parallel uploads would make "the one I just added"
ambiguous.
