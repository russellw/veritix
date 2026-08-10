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

Branch `build/audit-engine`, three commits on top of `Initial commit`. Not
merged to `main`. M0, M1, and M2 are done; M3 is next.

| | | |
|---|---|---|
| M0 | Skeleton, CLI, config, CI | done |
| M1 | Ingest and profile CSV/Excel into DuckDB | done |
| M2 | Checks, relationships, rules, reports | done |
| M3 | HTTP server and React web interface | **next** |
| M4 | Agentic LLM auditor with the egress guard | |
| M5 | MCP server and client | |
| M6 | Hardening, evals, deployment | |

Working today:

```sh
make build test lint
./bin/veritix audit testdata/dirty-retail
./bin/veritix audit testdata/dirty-retail --rules testdata/dirty-retail/veritix-rules.yaml
./bin/veritix audit testdata/dirty-retail --format html -o /tmp/report.html
./bin/veritix audit testdata/dirty-retail --format sarif
./bin/veritix audit testdata/dirty-retail --fail-on error   # exits 1
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
asserts this across all four formats. M4's egress guard extends the same idea
to the model.

## Package map

```
cmd/veritix            main
internal/
  cli/                 cobra commands: audit, serve (stub), version
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
testdata/dirty-retail/ fixtures with a known defect manifest
```

`audit.Run` is the single pipeline: discover → engine → ingest → profile →
checks → rules → verify. The CLI, and later the HTTP API and MCP server, all
call it. Three entry points assembling the pipeline slightly differently is how
a tool ends up reporting different results depending on how it was invoked.

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

## Testing

`testdata/dirty-retail/` carries deliberately broken files. The defect
manifest in `internal/checks/checks_test.go` lists **21 planted defects**, each
named with the check that must catch it, plus a companion list of places the
data is clean that must stay quiet — a check that fires on everything is
useless. Add to both lists when adding a check.

`sales.xlsx` is a committed binary fixture (title rows, a hidden row, merged
cells, `#REF!`/`#DIV/0!`, a stacked TOTAL table, a hidden sheet). It was
generated by a throwaway program; regenerate by hand if it ever needs changing.

Run `go test -race ./...` before committing — `profile` and `ingest` both fan
out across goroutines.

## Notes on working here

- Commit only when asked. Work happens on a branch, not `main`.
- The DuckDB driver needs CGO; prebuilt static libraries ship with the module,
  so plain `go build` works and the binary is ~61 MB with nothing to install.
- Module path is `github.com/russellwallace/veritix`. Licence AGPL-3.0.
