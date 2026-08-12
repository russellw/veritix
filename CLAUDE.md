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

Everything is on `main`. M0 through M3 are done.

| | | |
|---|---|---|
| M0 | Skeleton, CLI, config, CI | done |
| M1 | Ingest and profile CSV/Excel into DuckDB | done |
| M2 | Checks, relationships, rules, reports | done |
| M3a | HTTP API, SSE, SQLite run store, `veritix serve` | done |
| M3b | React web interface, embedded and served behind a CSP | done |
| M4 | Agentic LLM auditor with the egress guard | done |
| M5 | MCP server and client | **next** |
| M6 | Hardening, evals, deployment | |

M4 is off by default: `llm.provider: none` is the complete deterministic
auditor, and over HTTP the agent is per-run (`"agent": true`) rather than a
server-wide setting.

Working today:

```sh
make build test lint
make web            # Vite → web/dist, embedded by web/embed.go
make release        # web then build: the binary that ships an interface
make audit          # typecheck, pnpm audit, go mod verify, govulncheck

./bin/veritix audit testdata/dirty-retail
./bin/veritix audit testdata/dirty-retail --rules testdata/dirty-retail/veritix-rules.yaml
./bin/veritix audit testdata/dirty-retail --format html -o /tmp/report.html
./bin/veritix audit testdata/dirty-retail --format sarif
./bin/veritix audit testdata/dirty-retail --fail-on error   # exits 1

./bin/veritix audit testdata/dirty-retail --llm anthropic
./bin/veritix audit testdata/dirty-retail \
    --llm openai-compatible --llm-base-url http://localhost:11434/v1 --llm-model qwen3
./bin/veritix audit testdata/dirty-retail --llm anthropic --trace-out trace.json

./bin/veritix serve                          # loopback, no token
./bin/veritix serve --addr 0.0.0.0:8080 --auth-token "$(openssl rand -hex 16)"
VERITIX_LLM_PROVIDER=anthropic ./bin/veritix serve   # offers the agent in the UI
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
curl -s  localhost:8080/api/v1/runs/$ID/trace | jq .steps     # what the model saw
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
that no longer reproduces is dropped. For the deterministic checks that is
nearly a tautology. For the agent it is the whole mechanism: it chooses what to
look at, but a number only reaches the report if the engine produces it.
**Do not weaken this.** It is what makes an agentic auditor sellable rather
than merely plausible.

**Data does not leave the process.** Reports omit verbatim cell values unless
`--include-values` is passed, and say so in the output. Columns are described
by derived *shapes* instead — `CUS-000001` becomes `XXX-999999`, precise enough
to reason about and useless for exfiltration. `TestDefaultReportContainsNoRawValues`
asserts this across all four formats, and `TestReportOmitsRawValuesByDefault`
asserts it again over HTTP. M4's egress guard extends the same idea to the
model.

The model is the fourth place this has to hold, and `internal/agent/redact` is
the single path to it. See "How the agent is put together" below.

There is exactly one exception, and it is deliberate:
`GET /runs/{id}/findings/{fid}/rows` returns the offending rows themselves,
because showing somebody the three bad rows is the most useful thing the UI can
do. It has to be asked for one finding at a time, it never appears in a list
response, and its results are not logged. **Do not add a second way to get at
raw values.**

The browser is the third place this has to hold, not an afterthought to the
first two. The web interface can call that endpoint, so a compromised npm
package in the bundle would be sitting next to the data the product exists to
keep in. That is why the interface has three runtime dependencies and why the
CSP sets `connect-src 'self'`: the page may talk to the server it came from and
to nothing else. `TestWebInterfaceIsServedUnderAStrictCSP` pins it.
`docs/frontend-stack.md` is the whole argument.

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
  agent/               the tool-calling loop, the system prompt, the trace
    llm/               provider-agnostic message and tool types
      anthropic/       Claude, through the official SDK
      openaicompat/    Ollama, vLLM, LM Studio: hand-written, no SDK exists
      llmtest/         a scripted model, so the loop is testable without one
    redact/            the egress guard: the only path from process to model
    tools/             what the model may touch; record_finding is its only output
web/                   React + TS + Vite → dist, //go:embed-ed; embed.go
testdata/dirty-retail/ fixtures with a known defect manifest
docs/frontend-stack.md the front end's dependency and supply-chain policy
LICENSING.md           the dual license: AGPL, or commercial terms
CLA.md                 the contributor agreement that makes the second possible
CONTRIBUTING.md        how to work on it, and the four things a patch must not do
```

`audit.Run` is the single pipeline: discover → engine → ingest → profile →
checks → rules → *lockdown → agent* → verify. The CLI and the HTTP API both call it, and the MCP
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
  `TestFindingIDsAreStableAcrossRuns` pins this. The web interface puts that id
  in the address bar for the expanded finding, which is the whole reason it was
  built that way.
- **The interface is served on `/`, the API is matched first.** A path that is
  not a built asset gets `index.html`, so a client-side route survives a reload.
  An unmatched path *under* `/api/v1` stops there and gets the same JSON error
  shape as everything else rather than falling through to the app — it used to
  reach `ServeMux`'s plain-text 404, which broke the one-error-shape promise.
- **`api.Options.Web` is injected, not imported.** `internal/api` takes an
  `fs.FS`; `internal/cli/serve.go` passes `web.FS()`. That keeps the API's tests
  free of a front-end build, so they can serve a stub and also test the binary
  built without an interface at all — which is what plain `go build` produces.
- **The source offer comes from the server, not the bundle.** `/health` returns
  `source_url` alongside the version, and the interface's footer renders it on
  every screen including the token gate — which is why it rides on the one
  unauthenticated endpoint. AGPL §13 puts the obligation on whoever modified
  Veritix and served it, so `server.source_url` is theirs to set
  (`VERITIX_SOURCE_URL`, or `-ldflags` on `buildinfo.SourceURL` for a fork that
  relinks); baking it into the JavaScript would have made compliance require
  Node. Empty removes the link, for a build shipped commercially. `Validate`
  refuses anything that is not `http`/`https`, because it becomes an `href` in
  the one page that can display customer rows.

## How the agent is put together

`internal/agent` is off unless a provider is configured, and `audit.Run` with a
nil `Agent` is exactly the auditor M2 shipped. Everything below exists to make
one claim true: **the model chooses what to look at, and the engine decides
what is true.**

- **`record_finding` is the agent's only output**, and it takes a claim plus the
  query that would demonstrate it. Veritix runs the query. The model must also
  state `affected_count`, and a disagreement records *nothing* — it hands back
  the real figure and asks for the finding again. Correcting the number quietly
  is not enough, because the title is model-authored prose that usually carries
  the figure: a finding headed "400 orders have a negative amount" above a
  count of 1 looks like Veritix vouching for the 400. A query returning zero
  records nothing either. What is recorded then goes through `Set.Verify` with
  everything else, so it is measured twice.
- **A check tool says whether what it measured is new**, in a `note` on its own
  result: `check_referential_integrity` and `check_candidate_key` look their
  defect up in `World.Known` and either name the deterministic rule that already
  covers it, or say that none does and to record it now. The brief already lists
  the known findings, and that was not enough — a local model was handed two
  unresolved references `relate.go` never proposes, said nothing, and spent its
  remaining budget elsewhere. A tool result is read where the evidence is, which
  is the same reason the count correction lives in `record_finding` rather than
  in the prompt. It is a nudge: the model still decides, the engine still
  decides the number, `Set.Verify` still has the last word.
- **The egress guard is enforced by two types, not by diligence.**
  `redact.Text` is the only string type that may hold customer content and only
  a `Guard` method makes one; `redact.Sealed` is the only thing the loop sends
  and only `Guard.Seal` makes one. `Seal` walks a result and refuses a string
  reached through an `any` — a query cell, anything whose type stopped saying
  what it holds. A new tool returning raw values does not compile into a leak;
  it fails to seal, at the point where it would have been sent. Use
  `Guard.Derived` for shapes the profiler already derived, so the "withheld"
  counters mean what they say.
- **The brief carries the whole profile**, from `tools.Registry.Overview`, which
  is the same renderer `describe_table` uses — so what a model is handed and
  what it can ask for cannot drift apart. Orientation used to cost eight of
  twenty-four steps on both local models measured; it now costs +540 tokens once.
  It goes through `Guard.Seal` like a tool result, because the brief is the only
  other path to the model and the guard has to be the whole of it. `overviewBudget`
  bounds it, and any table that does not fit is named in `described_on_request`
  rather than silently dropped.
- **A shape sent to the model is delimited: `⟨XXX-999999⟩`.** A bare shape sits
  in a tool result exactly where a value would sit and looks like one, and two
  models eight times apart in size both read them as contents and queried for
  them — the larger spent its last seven steps on `WHERE region = 'XXXX'`,
  matching nothing every time. The system prompt says what a shape is and does
  not survive twenty steps of a filling context; the brackets travel with the
  shape. `redact.Mark` is the only thing that applies them and `Guard.Sentinel`
  is the exception, because `n/a` and `-` are real contents from a fixed
  vocabulary and `WHERE status = 'n/a'` is a query worth writing. Reports are
  unaffected — `internal/report` does not import `redact`, and a shape shown to
  a customer alongside their own data needs no such warning.
- **DuckDB errors are scrubbed of single-quoted content.** "Could not convert
  string 'N/A' to INT" is a cell value escaping through a diagnostic.
- **`Engine.Lockdown` runs before the agent starts**, from `audit.Run` rather
  than from a caller who has to remember. `enable_external_access=false` then
  `lock_configuration=true`, irreversibly, so `read_text('/etc/passwd')` and
  `COPY … TO` are refused by DuckDB rather than by Veritix's opinion of what a
  dangerous statement looks like.
- **`Engine.AnalyzeSelect` parses agent SQL with DuckDB's own parser**
  (`json_serialize_sql`). Anything that is not one SELECT fails to serialize.
  The select list is classified against DuckDB's aggregate catalog: aggregates
  come back as numbers, everything else as shapes. An expression built only from
  aggregates and constants counts as a statistic; anything unrecognized is
  treated as a value, which is the safe direction.
- **No model-supplied identifier reaches SQL.** A table or column name is looked
  up in the profile and the *profile's* name is what gets quoted.
- **The trace is a product feature.** It records every payload verbatim on both
  sides, is served at `/runs/{id}/trace`, and is written by `audit --trace-out`.
  It is how a customer checks the egress promise instead of taking it on trust,
  which is why nothing in it is summarized. Both entry points emit the same
  document — the CLI encodes `audit.Result.Trace`, which is what the API stores
  — so there is one answer to "what was the model sent", not two.
- **A model that misbehaves is not an error.** Bad arguments, refused SQL, a
  finding that does not reproduce — all come back to the model as tool errors so
  it can correct itself. A run ends when the model stops or a budget does.

The honest limit, stated in `redact`'s doc comment: the guard bounds what
Veritix *sends*. It is not a defense against a model deliberately smuggling data
out through carefully chosen aggregates. The guarantee is that ordinary
operation discloses no cell values, and that everything sent is in the trace.

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
- **US spelling everywhere**: prose, comments, identifiers, JSON fields, enum
  values, CSS classes. `analyze`, `normalize`, `center`, `canceled`, `license`,
  `catalog`, `behavior`. The repo used to mix conventions and one consistent
  answer is worth more than either argument; it also matches Go, CSS and SPDX,
  which are not going to change to suit us. The exception is `LICENSE`, which is
  the FSF's text verbatim.
- Comments explain *why*. The code says what.

## Gotchas already paid for

Each of these cost a debugging cycle; the fix is in the code with a comment.

**DuckDB**
- `sniff_csv` returns the literal string `(empty)`, not an empty string, for
  unset dialect options. Passing it back is rejected as an over-long quote.
  Normalized in `source.CSVDialect.normalize`.
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
  a `//nolint:gosec` naming the guard that makes it safe — sanitized name plus
  generated id, base-name-only, or a `DataDir` prefix check. Do not add a bare
  nolint; if there is no guard to name, there is a bug.
- Repo-wide lint is **clean, and should stay that way**. CI pins
  `golangci-lint` at `version: latest`, so a new release can introduce findings
  in code nobody touched. When that happens, fix the code rather than widening
  `.golangci.yml`; the sixteen findings cleared in "Answer the linter properly"
  were all worth fixing, and two of them were real (`WriteText` reporting
  success after its output had gone nowhere, and `os.Exit` skipping `main`'s
  deferred signal cleanup).

**The agent**
- `json_serialize_sql` returns a JSON *object*, so it needs a
  `CAST(... AS VARCHAR)` before `database/sql` will scan it into a string.
- `count(*)` appears in the parse tree as `count_star`, which is why the
  aggregate set is read from `duckdb_functions()` rather than written down.
- A UNION has no `select_list` at the top level, so `AnalyzeSelect` reports no
  aggregate columns and every cell is shaped. That is the intended failure
  direction and worth keeping if the parse-tree reader is ever extended.
- `max(name)` is an aggregate but returns a cell value, so `Guard.Cell` treats
  every string as a value regardless of the aggregate flag. Do not "optimize"
  that away.
- Shapes are fixed points of the shape function (`shape("XXX-999") == "XXX-999"`),
  which is why `Guard.Derived` can wrap a profiler shape without re-shaping it.
  The delimiters go on afterwards, at the boundary, so the property still holds
  of the bare pattern.

**Local models** — `scripts/local-model.sh` runs one by hand (probe, audit,
trace summary, egress check; `--probe`, `--serve`, `-- <veritix flags>`), and
`docs/local-model.md` is the whole setup. It is a script rather than a `make`
target on purpose: nothing should run a twenty-minute nondeterministic model by
accident, which is not a reason to make it tedious.
- Ollama sizes its context window from VRAM and picks **4096 tokens** when there
  is no GPU. Veritix's first agent prompt is ~4080 since the profile moved into
  the brief, so it does not fit at all; even when it did, llama.cpp discarded
  from the front within a step or two — taking the system prompt with it. The model stops knowing it may not see cell values and starts
  answering in prose, which reads as a stupid model rather than a truncated
  context. `OLLAMA_CONTEXT_LENGTH=32768` before `ollama serve`, always.
- Take a **non-thinking** model (Qwen3's `2507` instruct tags). A hybrid emits a
  reasoning block before every tool call, which on a CPU costs the same per
  token as useful output, and `openaicompat` drops it on the way back anyway.
- Probe `/v1/chat/completions` with a two-tool payload before running a full
  audit: twenty seconds to learn what a full run takes twenty minutes to prove.
- A small model does not ration its step budget — the first run here spent six
  consecutive steps on `describe_table` and finished with nothing recorded, and
  two more 12-step runs did the same. Budget for the model, not the dataset:
  the script defaults to 24 steps and a 30-minute per-call timeout, since a
  longer run reaches the slow full-context steps that outrun the product's
  10-minute default and would otherwise end on `provider_error`.

**Uploads**
- The upload directory used to be named with the first eight characters of a
  UUIDv7, and the comment called them random. They are the high bits of the
  millisecond timestamp and do not change for about a minute, so two uploads of
  the same folder within a minute shared a directory; `MkdirAll` returned the
  existing one happily and the upload failed later on the first file that was
  already there. It is `os.Mkdir` with a `crypto/rand` suffix now, so the
  uniqueness is structural. Found by the browser tests, once a second spec
  uploaded the same fixture.

**Web build**
- Vite's `emptyOutDir` wipes `web/dist/.gitkeep`, and without that placeholder
  `//go:embed all:dist` stops compiling on a clean checkout. `make web`
  re-`touch`es it; do not remove that line.
- `//go:embed all:dist` needs the `all:` prefix. A plain `dist` pattern skips
  files starting with `.`, which is exactly the placeholder.
- `web/.npmrc` sets `frozen-lockfile=true`, so the very first install with no
  lockfile has to be bootstrapped once with
  `corepack pnpm install --no-frozen-lockfile`. After that the committed
  lockfile is the source of truth.
- The 7-day `minimumReleaseAge` cooldown means a just-published version is
  refused. That is the control working. Dependencies are pinned to exact
  versions older than the window — at the time of writing that meant Vite 8.2.0
  rather than 8.2.1, which was a day old.

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

The web interface is tested from two sides. `internal/api/spa_test.go` covers how
it is *served* — the CSP, the client-side-route fallback, asset caching, that the
API is not shadowed, and the binary built without an interface — against a
`fstest.MapFS` stub rather than a real build, so `go test` never depends on
`make web` having been run. `web/embed_test.go` checks the real bundle when one
is present and skips when it is not.

`internal/agent`'s tests drive the real loop over the same fixtures with a
scripted provider (`llm/llmtest`): every outbound payload is scanned for the
same values `rawValuesInFixture` lists in the report tests, nine escape attempts
are refused, an inflated claim is corrected, an invented finding is declined,
and both budgets stop a runaway. `internal/api` goes further and points the
server at a real HTTP endpoint speaking chat-completions, so the provider, the
loop, the guard, the store and the handler are all on the tested path — that is
`stubModel` in `agent_test.go`, and the browser tests use the same idea through
`e2e/stub-model.mjs`. No test calls a real model: they are about what Veritix
does with what a model said.

`e2e/` covers what happens once a browser executes it: Playwright against the Go
binary serving the embedded build. `make e2e` builds, serves on a throwaway data
directory, runs the suite and tears it all down. It is a separate pnpm workspace
with its own lockfile so Playwright never enters the shipped interface's
dependency tree, and the browser download is an explicit step rather than an
install script — see `e2e/README.md` and `docs/frontend-stack.md` §8. There is no
JavaScript unit-test runner; that is a dependency the current UI does not earn.

`make e2e` also starts `e2e/stub-model.mjs`, a scripted chat-completions
endpoint, and points the server at it with `VERITIX_LLM_*`, so the agentic
screens can be driven without a network model. `scripts/local-model.sh` is the
other half of that: the same run against a real one, by hand, when the question
is whether a model that was not scripted can actually do the job. Pointing the
script at the stub (`MODEL=stub-model BASE_URL=http://127.0.0.1:11435/v1`) is
also how to check the script itself without waiting twenty minutes.

Running the browser tests needs system packages once, and they need root:
`sudo apt-get install -y libasound2t64 libatk1.0-0t64 libatk-bridge2.0-0t64
libatspi2.0-0t64 libgbm1 libxcomposite1 libxdamage1 libxfixes3 libxrandr2
fonts-liberation`. `playwright install-deps` also works but pulls several
hundred packages a headless run never uses. **Do not drop the font package**:
with no fonts installed Chromium starts fine and renders every page with no
glyphs, which presents as a CSS bug — right layout, right colors, invisible
text — rather than as a missing dependency.

## Notes on working here

- Work goes straight onto `main` — this is a one-person project, and a branch
  here buys review that nobody is going to do. Do not create feature branches.
  Use your judgment about when a piece of work is worth committing.
- The DuckDB driver needs CGO; prebuilt static libraries ship with the module,
  so plain `go build` works and the binary is ~71 MB with nothing to install.
  The SQLite driver is `modernc.org/sqlite`, pure Go on purpose: a second C
  library in the build is a second way to break the one CGO dependency that
  actually matters.
- **Node is a build-time requirement only.** Node 24 (pinned in `web/.nvmrc`)
  and pnpm 11 (pinned in `web/package.json`'s `packageManager`, fetched by
  corepack). Installed here under `~/.local/lib/node` from the official tarball
  with its SHA-256 checked, symlinked into `~/.local/bin`. The shipped binary
  contains no Node and needs none.
- **Go modules are deliberately not vendored.** `vendor/` measures 728 MB, 572
  of it five prebuilt DuckDB static libraries whose diffs nobody can review, so
  it would buy no auditability and add that much to git history on every bump.
  `go mod verify` plus `govulncheck` stand in for it, both in CI and in
  `make audit`. The reasoning is in `docs/frontend-stack.md` §6.
- `go.mod` carries an explicit `toolchain` line. It is there because
  `govulncheck` reported reachable standard-library vulnerabilities fixed only
  in a later patch release; bump it rather than silence the check.
- `golangci-lint` is not in the `Makefile`'s dependency set. Install it with
  `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest`;
  `make lint` falls back to `go vet` alone without it, which will not catch
  what CI catches.
- **The Anthropic SDK is the one dependency M4 added** — eleven modules with
  its transitive set, measured before adopting it, reasoning in
  `docs/frontend-stack.md` §6.1. The OpenAI-compatible provider is hand-written
  and stays that way: there is no official SDK for "whatever Ollama is serving
  today", and the servers implementing that dialect disagree about corners a
  client written for the reference implementation would hide.
- A default install talks to nobody. `llm.provider` is `none`, and both entry
  points make turning it on a deliberate act: `--llm` on the CLI, `"agent": true`
  per run over HTTP.
- Module path is `github.com/russellw/veritix`.
- **Dual licensed on purpose**: AGPL-3.0-or-later, or commercial terms for
  customers who cannot take the AGPL — `LICENSING.md` says which is which, and
  it is a selling document as much as a legal one. Two consequences for the
  code. First, **a copyleft dependency is not adoptable at any technical
  merit**: everything linked or embedded today is MIT, BSD-3-Clause or
  Apache-2.0, and a GPL/AGPL/SSPL library would be a term the commercial
  license could not deliver. Check the license before measuring the module
  count. Second, contributions from anyone but the copyright holder need the
  CLA in `CLA.md`, signed by a `Signed-off-by` trailer — code without one
  cannot go into a commercially licensed build, and merging it anyway is how a
  dual license quietly stops being true.
