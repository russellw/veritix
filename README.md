# Veritix

Veritix audits datasets. Point it at a pile of CSV files, an Excel workbook, or
(later) a SQL database, and it profiles every column, verifies integrity within
and across files, and reports the inconsistencies and likely problems it finds.

**It runs on your hardware.** Veritix is a program you run locally or deploy to
your own cloud, not a service you upload data to. There is no vendor in the
middle of your commercially sensitive data.

## Status

Early development. See `.claude/plans/` for the build plan; the milestones are:

| | |
|---|---|
| M0 | Skeleton, CLI, config, CI — **done** |
| M1 | Ingest and profile CSV/Excel into DuckDB — **done** |
| M2 | Deterministic checks, relationships, rules, reports — **done** |
| M3 | HTTP server and React web interface — **done** |
| M4 | Agentic LLM auditor with a strict data-egress guard — **done** |
| M5a | MCP server: audit datasets from Claude Code or Claude Desktop — **done** |
| M5b | MCP client: pull your own context into an audit — **done** |
| M6 | Hardening, evals, deployment, rule proposal — **done** |
| M7 | Run-over-run comparison and scheduled audits — **done** |
| M8 | Windows: built, tested and shipped on the platform the interface is for — **done** |

## Design in one page

**A directory is one dataset, not a pile of files.** Real business data arrives
as a folder of exports that reference each other. Veritix infers the keys that
join them and checks that those joins actually hold, which is where most real
problems live.

**DuckDB does the measuring.** Files are read in place into an embedded
columnar engine, and profiles and checks are SQL aggregates over it. DuckDB is
statically linked into the binary — there is nothing to install.

**The model explores; the engine measures.** The agentic auditor decides *what*
to investigate and writes the explanation, but it never reports a number it
made up. To record a finding it supplies the query that would demonstrate it
and states what it expects that query to return; Veritix runs the query, and a
disagreement records nothing and hands back the real figure. Everything
recorded is then re-run again with the deterministic findings before the report
is written. A finding either reproduces or it is dropped.

**Findings carry their evidence.** Every finding names the query that produced
it, so a reader can check the claim rather than take it on trust — which is
what lets model-proposed findings sit in the same list as deterministic ones.

**Your data does not leave the process.** With a cloud model provider, the
agent sees schemas, aggregates, distributions, and value *shapes* —
`CUS-004417` reaches it as `XXX-999999` — never cell values. Sending samples
requires an explicit opt-in, and even then they are masked and truncated first.
Afterwards you can read every payload that left the machine, verbatim, on the
run's trace. A local model (Ollama, vLLM, LM Studio) is a first-class option
for customers who want no network egress at all, and no model at all is the
default: Veritix without one is a complete deterministic auditor.

## Installing

Download the archive for your platform from the releases page, unzip it, and
run it — there is no installer, no runtime to install alongside it, and
nothing to register.

On **Windows**, double-click **Start Veritix**: it starts the server and opens
your browser on the interface, which is the whole of the setup.
[docs/windows.md](docs/windows.md) is the rest — where your data lives, what
SmartScreen will say about an unsigned executable, and why a scheduled audit
knows what `Europe/London` means on a platform that does not ship the zone
database.

On **Linux**, `veritix serve --open` does the same thing, and
[docs/deployment.md](docs/deployment.md) covers the container and the cluster.

## Building

Requires Go 1.26+ and a C toolchain (DuckDB is a C++ library; its prebuilt
static libraries ship with the Go module, so there is nothing else to install).
Building the web interface also needs Node 24 and pnpm — at build time only.
The binary Veritix ships contains no Node and needs none to run it.

```sh
make build      # → bin/veritix, embedding whatever is in web/dist
make web        # build the web interface into web/dist
make release    # web, then build: the binary that ships an interface
make test       # unit and golden-file tests
make lint       # go vet plus golangci-lint if present
make audit      # dependency checks: pnpm audit, go mod verify, govulncheck
make e2e        # browser tests against the embedded build (see e2e/README.md)
```

The front end has three runtime dependencies — `react`, `react-dom` and
`scheduler` — and is served behind a strict Content-Security-Policy that lets
the page talk to the Veritix server and nowhere else. That is not incidental:
the interface can display a finding's offending rows, so it sits next to exactly
the data this product exists to keep in. `docs/frontend-stack.md` is the whole
argument, including what the policy does not solve.

## Usage

```sh
# Audit a dataset from the shell or CI
veritix audit ./data
veritix audit ./data --format json
veritix audit ./data --format html -o report.html
veritix audit ./data --format sarif -o veritix.sarif   # for code scanning
veritix audit ./data --rules my-expectations.yaml
veritix audit ./data --fail-on error                   # non-zero exit for CI

# What changed since the last audit (see docs/comparison.md)
veritix audit ./data --baseline last-report.json
veritix audit ./data --baseline last-report.json --fail-on-regression error

# Serve it to an assistant over MCP (see docs/mcp.md)
veritix mcp --data-dir ~/.veritix

# Run the server and web interface (loopback by default)
veritix serve
veritix serve --addr 0.0.0.0:8080 --auth-token "$TOKEN"

# From there, a dataset can audit itself every night and say when it got
# worse — set it on the dataset screen, or see docs/scheduling.md
```

Configuration comes from `./veritix.yaml`, then `VERITIX_*` environment
variables, then flags. See `internal/config/config.go` for every field.

## Running a model that is larger than your RAM

A local model is the answer for anyone who wants no network egress at all, and
*local* does not have to mean *small*. Measured here: `gpt-oss-120b` — 63 GB of
weights — auditing `testdata/dirty-retail` on a 30 GB laptop with a SATA SSD and
no GPU, finishing a complete run in **59 minutes** with two verified findings,
both of them unresolved foreign-key references that the deterministic pass does
not propose. The weights are paged off the disk as they are needed.

Five settings decide whether that works or fails, and four of them fail
*silently* — as a slow model, or as a run that ends on `provider_error` having
recorded nothing:

| | |
|---|---|
| `--no-repack --fit off --load-mode mmap` | serve with llama.cpp, not Ollama. Weight repacking materializes tensors in anonymous memory and so defeats mmap by construction, and there is no quantization that avoids it. Confirm it took with `VmSize` ≫ `VmRSS`. |
| `--ubatch-size 2048` | the default of 512 is wrong for a model that does not fit. A micro-batch reads each expert once and uses it for every token in the batch, so its size divides the I/O outright: **1.7x** on the first agent step. |
| `--parallel 1` | with several slots a follow-up turn can land on a cold one and re-prefill the whole conversation, which here is half an hour. |
| `llm.request_timeout` | the first step is nearly half the wall clock, because an agent's brief is a long prompt and all of it is prefilled before a single token comes back. The product default of 10 minutes expires in the middle of it. |
| `--llm-effort low` | **not `none`** — gpt-oss's template knows `low`/`medium`/`high` and quietly defaults anything else. Applied, it took one run from 6h47m to 1h5m. |

`scripts/local-model.sh` sets all of these and starts the server itself, which
is why it exists: a recipe that has to be remembered in another terminal is a
recipe that will one day be typed without `--no-repack`.
[docs/local-model.md](docs/local-model.md) has the measurements and the traces.

**What is not on that list is expert prefetch**, and it is worth saying so
because the obvious reading of the literature is that it should be. Custom
out-of-core code — issuing one large read per expert instead of letting the
kernel fault pages in 4 KiB at a time — is genuinely worth 1.6-1.8x on
generation when a model does not fit, and
[big-local-llms](https://github.com/russellw/big-local-llms) measures it
carefully. It buys Veritix almost nothing, and the reason generalizes to any
agent rather than being a fact about this one.

Prefetch pays only while the set it advises survives in RAM until it is read.
Expert routing saturates fast — most of the pool within a few dozen tokens — so
a single generated token advises a set that fits, and a prefill micro-batch
advises one that does not: on this brief, 317 GB of advice into 30 GB of RAM.
Pages fetched early in a layer are evicted before the arithmetic reaches them,
and the re-reads cost more than the saved faults. An agent request is ~99%
prefill, so a 1.3x on generation is worth well under 1% of a call. **Batching
and caching attack the same waste** — each expert read once, used many times —
which is also why `--ubatch-size` and prefetch do not stack, and why the flag
is on the list above and the hook is not.

## Deploying it

```sh
make docker                          # a distroless image, interface included
kubectl apply -k deploy/kubernetes    # one replica, egress denied by default
```

`docs/deployment.md` covers all three shapes — a binary on a desktop, a
container, a cluster — and why the Kubernetes base runs one replica and denies
egress. Nothing here reaches a network Veritix was not told about: the model
provider is `none` until configured, OpenTelemetry export is off until enabled,
and a scheduled audit tells nobody until a webhook is set.

## Documentation

| | |
|---|---|
| [docs/checks.md](docs/checks.md) | every deterministic check, what each one reports, and why it matters downstream |
| [docs/deployment.md](docs/deployment.md) | running it: binary, container, cluster, CI, telemetry |
| [docs/comparison.md](docs/comparison.md) | what changed since the last audit, and failing a build on the direction rather than the state |
| [docs/scheduling.md](docs/scheduling.md) | auditing on a clock, being told when the export gets worse, and keeping the disk |
| [docs/rules-proposal.md](docs/rules-proposal.md) | the model proposes a rule, a person accepts it, every later audit enforces it |
| [docs/eval.md](docs/eval.md) | scoring an audit against known defects, and why one run is not a measurement |
| [docs/scale.md](docs/scale.md) | what a two-gigabyte dataset costs, and the four things it found |
| [docs/mcp.md](docs/mcp.md) | wiring an assistant to `veritix mcp` |
| [docs/local-model.md](docs/local-model.md) | running the agent against a model on your own hardware |
| [docs/frontend-stack.md](docs/frontend-stack.md) | the dependency and supply-chain policy, both sides of the build |
| [docs/windows.md](docs/windows.md) | the platform the interface is for: getting started, where the data lives, and what is not there |

## License

Veritix is dual licensed: **AGPL-3.0-or-later** (the full text is in
`LICENSE`), or a **commercial license** for anyone who needs terms the AGPL
cannot give — shipping it inside a product of their own, running a modified
copy as a service without publishing the modifications, or getting a warranty
and a support commitment on paper.

Same code either way. `LICENSING.md` explains which one you need and how to
ask about the second. Contributions are accepted under the CLA in `CLA.md`;
`CONTRIBUTING.md` has the mechanics.
