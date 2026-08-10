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
| M1 | Ingest and profile CSV/Excel into DuckDB |
| M2 | Deterministic checks, relationships, rules, reports |
| M3 | HTTP server and React web interface |
| M4 | Agentic LLM auditor with a strict data-egress guard |
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
to investigate, but it never reports a number it made up: every finding it
records carries the query that produced it, and that query is re-run
deterministically when the report is built. A finding either reproduces or it
is dropped.

**Your data does not leave the process.** With a cloud model provider, the
agent sees schemas, aggregates, distributions, and pattern signatures — never
cell values. Sending samples requires an explicit opt-in, and even then they
pass through PII redaction first. A local model (Ollama, vLLM, llama.cpp) is a
first-class option for customers who want no network egress at all.

## Building

Requires Go 1.25+ and a C toolchain (DuckDB is a C++ library; its prebuilt
static libraries ship with the Go module, so there is nothing else to install).

```sh
make build      # → bin/veritix
make test       # unit and golden-file tests
make lint       # go vet plus golangci-lint if present
```

## Usage

```sh
# Audit a dataset from the shell or CI
veritix audit ./data --format json
veritix audit ./data --fail-on error

# Run the server and web interface (loopback by default)
veritix serve
veritix serve --addr 0.0.0.0:8080 --auth-token "$TOKEN"
```

Configuration comes from `./veritix.yaml`, then `VERITIX_*` environment
variables, then flags. See `internal/config/config.go` for every field.

## Licence

AGPL-3.0. See `LICENSE`.
