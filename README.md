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
| M5 | MCP server and client |
| M6 | Hardening, evals, deployment |

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

# Run the server and web interface (loopback by default)
veritix serve
veritix serve --addr 0.0.0.0:8080 --auth-token "$TOKEN"
```

Configuration comes from `./veritix.yaml`, then `VERITIX_*` environment
variables, then flags. See `internal/config/config.go` for every field.

## Licence

AGPL-3.0. See `LICENSE`.
