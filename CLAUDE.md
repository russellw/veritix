# Veritix — working notes

Orientation for anyone (human or model) picking this up cold. The build plan
lives at `~/.claude/plans/memoized-riding-knuth.md`; this file covers how the
code is put together and what is easy to get wrong.

## What this is

A tool that audits datasets — CSV, Excel, later SQL databases — verifies their
integrity, and reports inconsistencies and likely problems.

It is **a program the customer runs**, on their own machine or their own cloud.
Not SaaS. The whole proposition is that commercially sensitive data never goes
to a vendor, and every design decision has to keep that true.

The **web interface is the primary interface**: the users are business people
on Windows desktops who do not use a Linux command line. The CLI exists for
development, scripting, and CI.

## Current state

Everything is on `main`. M0 through M2 are done, and M3's server half is done;
the web interface is what remains of M3.

| | | |
|---|---|---|
| M0 | Skeleton, CLI, config, CI | done |
| M1 | Ingest and profile CSV/Excel into DuckDB | done |
| M2 | Checks, relationships, rules, reports | done |
| M3a | HTTP API, SSE, SQLite run store, `veritix serve` | done |
| M3b | React web interface | **next** |
| M4 | Agentic LLM auditor with the egress guard | |
| M5 | MCP server and client | |
| M6 | Hardening, evals, deployment | |

**M3b needs a Node toolchain that is not installed on this machine** — no
`node`, `npm`, `pnpm`, `yarn` or `bun`, no `web` target in the `Makefile`, and
no Node step in `.github/workflows/ci.yml`. The product stays a single binary
with no Node at *runtime*, but Vite has to run at build time. Install it before
starting the SPA, and add both the Makefile target and the CI step in the same
change.

Working today:

```sh
make build test lint
./bin/veritix audit testdata/dirty-retail
./bin/veritix audit testdata/dirty-retail --rules testdata/dirty-retail/veritix-rules.yaml
./bin/veritix audit testdata/dirty-retail --format html -o /tmp/report.html
./bin/veritix audit testdata/dirty-retail --format sarif
./bin/veritix audit testdata/dirty-retail --fail-on error   # exits 1

./bin/veritix serve                          # loopback, no token
./bin/veritix serve --addr 0.0.0.0:8080 --auth-token "$(openssl rand -hex 16)"
```

Driving the API by hand:

```sh
curl -s localhost:8080/api/v1/health
DS=$(curl -s -XPOST localhost:8080/api/v1/datasets -H 'Content-Type: application/json' \
      -d "{\"path\":\"$PWD/testdata/dirty-retail\"}" | jq -r .id)
ID=$(curl -s -XPOST localhost:8080/api/v1/runs -H 'Content-Type: application/json' \
      -d "{\"dataset_id\":\"$DS\"}" | jq -r .id)
curl -sN localhost:8080/api/v1/runs/$ID/events            # progress, then done
curl -s  localhost:8080/api/v1/runs/$ID/report | jq .finding_summary
curl -s  localhost:8080/api/v1/runs/$ID/findings/<fid>/rows   # the gated one
```

## Four ideas the design rests on

**A directory is one dataset, not a pile of files.** Business data arrives as a
folder of exports that reference each other, and most real defects live in the
relationships between them rather than inside any one file.

**Everything loads as text.** A normal import guesses a type per column and
silently discards whatever does not fit — a stray `N/A` in a numeric column
becomes `NULL`. Those discarded values are exactly what an auditor needs. So
`ingest` declares every column `VARCHAR`, and `profile` works out the types
afterwards and reports the difference between what a column claims to be and
what it holds.

**Findings carry re-runnable evidence.** Every finding has a `CountQuery`, and
`finding.Set.Verify` re-runs all of them before anything is reported; a finding
that no longer reproduces is dropped. Right now that is nearly a tautology,
because the checks are deterministic. It exists for M4: the agent chooses what
to look at, but a number only reaches the report if the engine produces it.
**Do not weaken this.** It is what makes an agentic auditor sellable rather
than merely plausible.

**Data does not leave the process.** Reports omit verbatim cell values unless
`--include-values` is passed, and say so in the output. Columns are described
by derived *shapes* instead — `CUS-000001` becomes `XXX-999999`, precise enough
to reason about and useless for exfiltration. `TestDefaultReportContainsNoRawValues`
asserts this across all four formats, and `TestReportOmitsRawValuesByDefault`
asserts it again over HTTP. M4's egress guard extends the same idea to the
model.

There is exactly one exception, and it is deliberate:
`GET /runs/{id}/findings/{fid}/rows` returns the offending rows themselves,
because showing somebody the three bad rows is the most useful thing the UI can
do. It has to be asked for one finding at a time, it never appears in a list
response, and its results are not logged. **Do not add a second way to get at
raw values.**

## Package map

```
cmd/veritix            main
internal/
  cli/                 cobra commands: audit, serve, version
  config/              defaults → YAML file → VERITIX_* env → flags
  telemetry/           slog setup (stderr; stdout stays clean for reports)
  engine/              DuckDB wrapper: limits, timeouts, SQL quoting, ResultSet
  source/              file discovery, CSV dialect+encoding sniffing, Excel reader
  ingest/              loads discovered files into DuckDB as VARCHAR, captures rejects
  profile/             per-column measurement and type inference
  checks/              profile → findings (column, table, cross-file relationships)
  rules/               customer-authored YAML expectations
  finding/             the finding model, severity, evidence, Set.Verify
  report/              text, JSON, SARIF, self-contained HTML
  audit/               the orchestrator every entry point drives
  store/               SQLite: datasets, runs, findings — the audit trail
  api/                 REST + SSE over audit.Run; openapi.yaml is the contract
testdata/dirty-retail/ fixtures with a known defect manifest
```

`audit.Run` is the single pipeline: discover → engine → ingest → profile →
checks → rules → verify. The CLI and the HTTP API both call it, and the MCP
server will. Three entry points assembling the pipeline slightly differently is
how a tool ends up reporting different results depending on how it was invoked.

## How the server is put together

- **The API serves the same `report.Document` the JSON report writes.** It is
  built once when the run finishes, stored as an opaque blob, and handed back
  verbatim; `report.RenderHTML` renders the download from that same document.
  The web UI and the JSON report cannot disagree because there is only one.
- **Two databases, on purpose.** DuckDB holds dataset content: large,
  disposable, re-creatable from the customer's files. SQLite holds the record
  of what was done: small, long-lived, the thing somebody wants six months
  later. `internal/store` knows nothing about the report's shape, so changing
  the report schema is not a migration.
- **A run outlives the request that started it.** `POST /runs` returns an id;
  the run executes on a background context and progress arrives over SSE. A
  closed browser tab must not abandon an audit. Cancellation is explicit.
- **Progress events are the pipeline's own log lines.** `internal/api`'s
  `progressHandler` wraps the `slog.Handler` handed to `audit.Run`, so a stage
  that is logged is a stage the browser sees. A second notification mechanism
  would drift from the first within a milestone.
- **Each run keeps its DuckDB file** at `<DataDir>/runs/<id>/dataset.duckdb`,
  so the rows endpoint can reopen it read-only afterwards. It is deleted when
  its dataset is. The engine is closed *before* the run is recorded as
  finished, because the file has to be flushed before anything reopens it.
- **`finding.Finding.ID()`** is a digest of the same key that de-duplicates
  findings, so it names a problem rather than a position: the id in a URL still
  points at the same finding after a re-run that turns up one more error.
  `TestFindingIDsAreStableAcrossRuns` pins this.

## Conventions

- **All SQL identifiers go through `engine.Ident`, literals through
  `engine.Literal`.** Column names come out of customer files and are untrusted
  input; a column can be named `"; DROP TABLE x; --` and still has to profile.
  `TestIdentQuotingResistsInjection` covers it. Bind parameters where DuckDB
  accepts them; table functions like `read_csv` do not, hence `Literal`.
- **`profile` exports its SQL predicates** (`SQLNonBlank`, `SQLMatchesKind`,
  `SQLIsSentinel`, `SQLAmbiguousDate`). `checks` and `rules` build evidence
  from those rather than writing their own. Two definitions of "is this a valid
  date" would eventually disagree, and then a finding would contradict the
  profile it came from.
- **Findings say why it matters downstream**, not what the check looked at.
  "signup_date has 2 date formats" is a fact; "some of these dates will be read
  as the wrong day and nothing will error" is what makes somebody act.
- **A check that cannot run must say so.** `rules` reports a rule that matched
  nothing (`rule.never_applied`) because silence is ambiguous — it means either
  "your data is fine" or "this never ran", and the second is dangerous when
  somebody is relying on it.
- Diagnostics to stderr, report to stdout.
- Comments explain *why*. The code says what.

## Gotchas already paid for

Each of these cost a debugging cycle; the fix is in the code with a comment.

**DuckDB**
- `sniff_csv` returns the literal string `(empty)`, not an empty string, for
  unset dialect options. Passing it back is rejected as an over-long quote.
  Normalised in `source.CSVDialect.normalise`.
- `TRY_CAST('89.99' AS BIGINT)` **succeeds**, truncating. Integers are matched
  on their written form via `regexp_full_match` instead.
- `new_line` wants the escaped text `\n`, not a real newline character.
- Strict sniffing refuses to describe a file with ragged rows at all. All
  sniffs pass `ignore_errors=true, null_padding=true`.
- Reject-table counters come back as `uint64`; `toInt64` handles the spread.
- `sql.Row` defers work to `Scan`, so a timeout released before `Scan` cancels
  the query. There is deliberately **no exported `QueryRow`** on `engine` —
  use `ScanOne`. Same reason `engine.Rows` owns its cancel func until `Close`.

**Delimiter detection**
DuckDB's sniffer chose `|` for a comma-separated file whose rows were ragged,
because a character appearing nowhere yields a perfectly consistent one column
per row. The file would have loaded as a single column and every column check
would have passed vacuously. `source/delimiter.go` scores candidates itself and
prefers one that actually splits the file. Related: the header line is treated
as the authority on column count, because one over-long row would otherwise
widen the schema and get every well-formed row rejected.

**excelize**
- `GetSheetDimension` returns `A1` for workbooks not written by Excel, so it
  cannot be used to find the sheet width. `source/excel.go` buffers the first
  100 rows instead (`findHeader`), which also locates a header sitting under
  title rows.
- `rows.GetRowOpts()` returns one value, not two, and gives hidden-row state
  for free during iteration.

**Relationship inference** (`checks/relate.go`) — two rules learned from false
positives, both commented in place:
- A column named as its own table's identifier is not a foreign key into
  anything (`namesOwnTable`).
- A key containing a duplicate is still the key. Requiring perfect uniqueness
  disqualified `customers.customer_id` and hid both the duplicate and the
  orphaned reference pointing at it.

**Server**
- `http.Server.Shutdown` waits for connections to go idle and does *not* cancel
  request contexts. An SSE stream is idle-never, so it would hold a graceful
  shutdown open for the whole timeout. `api.Server.Close` is therefore called
  **before** `Shutdown`: it closes a `stopping` channel the event handlers
  select on, and only then does the drain finish quickly.
- A run recorded as `running` that survives a restart belongs to a process that
  is gone. `store.MarkInterrupted` closes those out at startup, or the history
  lies and an events stream waits forever on nothing.
- `golangci-lint`'s `gosec` taint analysis (G703/G304) flags every
  filesystem call reachable from a request. Each one in `internal/api` carries
  a `//nolint:gosec` naming the guard that makes it safe — sanitised name plus
  generated id, base-name-only, or a `DataDir` prefix check. Do not add a bare
  nolint; if there is no guard to name, there is a bug.
- Repo-wide lint has **16 pre-existing findings on `HEAD`** (errcheck in
  `report`, revive on some exported consts, and so on) from linter versions
  drifting since M2. CI pins `version: latest`, so it will report them too.
  They are not new; check a baseline before assuming a change introduced one.

## Testing

`testdata/dirty-retail/` carries deliberately broken files. The defect
manifest in `internal/checks/checks_test.go` lists **21 planted defects**, each
named with the check that must catch it, plus a companion list of places the
data is clean that must stay quiet — a check that fires on everything is
useless. Add to both lists when adding a check.

`sales.xlsx` is a committed binary fixture (title rows, a hidden row, merged
cells, `#REF!`/`#DIV/0!`, a stacked TOTAL table, a hidden sheet). It was
generated by a throwaway program; regenerate by hand if it ever needs changing.

`internal/api`'s tests drive the real pipeline over a real `httptest` server
against those same fixtures, rather than stubbing `audit.Run`. The API's whole
job is to expose that pipeline faithfully, and a fake would only test the fake.

Run `go test -race ./...` before committing — `profile` and `ingest` fan out
across goroutines, and `api` now runs audits on background goroutines while
serving.

## Notes on working here

- Commit only when asked. Work goes straight onto `main` — this is a one-person
  project, and a branch here buys review that nobody is going to do. Do not
  create feature branches.
- The DuckDB driver needs CGO; prebuilt static libraries ship with the module,
  so plain `go build` works and the binary is ~61 MB with nothing to install.
  The SQLite driver is `modernc.org/sqlite`, pure Go on purpose: a second C
  library in the build is a second way to break the one CGO dependency that
  actually matters.
- `golangci-lint` is not in the `Makefile`'s dependency set. Install it with
  `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`;
  `make lint` falls back to `go vet` alone without it, which will not catch
  what CI catches.
- Module path is `github.com/russellwallace/veritix`. Licence AGPL-3.0.
