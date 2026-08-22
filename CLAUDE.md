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
| M5a | MCP server: `veritix mcp` on stdio | done |
| M5b | MCP client: the agent pulls the customer's own context | |
| M6a | The eval harness: defect manifests, `veritix eval`, a second fixture | done |
| M6b | Rule proposal, OpenTelemetry, deployment, docs | done |

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

make eval                                    # score the checks against the manifest
go run ./scripts/gen-dataset -out /var/tmp/vx-big -scale 1   # 2 GB and a manifest
./bin/veritix eval /var/tmp/vx-big           # the same score, at that size
./bin/veritix eval testdata/dirty-logistics --llm anthropic --runs 5
./bin/veritix eval testdata/dirty-logistics --rules accepted.yaml  # what a rule bought

./bin/veritix audit testdata/dirty-retail --llm anthropic
./bin/veritix audit testdata/dirty-retail \
    --llm openai-compatible --llm-base-url http://localhost:11434/v1 --llm-model qwen3
./bin/veritix audit testdata/dirty-retail --llm anthropic --trace-out trace.json
./bin/veritix audit testdata/dirty-retail --llm anthropic \
    --propose-rules-out proposed.yaml   # rules to review, then load with --rules

./bin/veritix serve                          # loopback, no token
./bin/veritix serve --addr 0.0.0.0:8080 --auth-token "$(openssl rand -hex 16)"
VERITIX_LLM_PROVIDER=anthropic ./bin/veritix serve   # offers the agent in the UI

./bin/veritix mcp                            # stdio; an assistant launches it
claude mcp add veritix -- "$PWD/bin/veritix" mcp --data-dir ~/.veritix

make docker                                  # the image, interface included
kubectl apply -k deploy/kubernetes            # one replica, egress denied

VERITIX_OTEL_ENABLED=true \
  VERITIX_OTEL_ENDPOINT=http://127.0.0.1:4318 ./bin/veritix audit testdata/dirty-retail
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
curl -s  localhost:8080/api/v1/runs/$ID/proposals | jq .        # rules suggested
curl -s  localhost:8080/api/v1/runs/$ID/proposals/<pid>          # the gated one
curl -s -XPOST localhost:8080/api/v1/datasets/$DS/rules -H 'Content-Type: application/json' \
     -d "{\"run_id\":\"$ID\",\"proposal_id\":\"<pid>\",\"values\":[\"Active\",\"Closed\"]}"
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

There are exactly two exceptions, both deliberate and both bounded the same
way. `GET /runs/{id}/findings/{fid}/rows` returns the offending rows
themselves, because showing somebody the three bad rows is the most useful
thing the UI can do. `GET /runs/{id}/proposals/{pid}` returns the values a
proposed `one_of` rule would permit, because that set is materialized from the
customer's own column and an accept screen that cannot show what it is about to
bless is not a review — it is how the misspelled status in `dirty-retail` gets
struck out instead of enforced forever. Each has to be asked for one named
thing at a time, neither ever appears in a list response, and neither's results
are logged. **Do not add a third.**

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
  telemetry/           slog setup, and OpenTelemetry: the exporters and the
                       one rule about what a span may carry
  engine/              DuckDB wrapper: limits, timeouts, SQL quoting, ResultSet
  source/              file discovery, CSV dialect+encoding sniffing, Excel reader
  ingest/              loads discovered files into DuckDB as VARCHAR, captures rejects
  profile/             per-column measurement and type inference
  checks/              profile → findings (column, table, cross-file relationships)
  rules/               customer-authored YAML expectations
  finding/             the finding model, severity, evidence, Set.Verify
  report/              text, JSON, SARIF, self-contained HTML
  audit/               the orchestrator every entry point drives
  eval/                score an audit against a dataset whose defects are known
  runs/                one recorded run: audit.Run plus the store bookkeeping
  store/               SQLite: datasets, runs, findings — the audit trail
  api/                 REST + SSE over audit.Run; openapi.yaml is the contract
  mcp/                 the Model Context Protocol server: `veritix mcp`
  agent/               the tool-calling loop, the system prompt, the trace
    llm/               provider-agnostic message and tool types
      anthropic/       Claude, through the official SDK
      openaicompat/    Ollama, vLLM, LM Studio: hand-written, no SDK exists
      llmtest/         a scripted model, so the loop is testable without one
    redact/            the egress guard: the only path from process to model
    tools/             what the model may touch; record_finding is its only output
web/                   React + TS + Vite → dist, //go:embed-ed; embed.go
testdata/dirty-retail/    fixtures with a known defect manifest
testdata/dirty-logistics/ a second one, whose defects need reasoning not a tool
deploy/Dockerfile      three stages: the interface, the binary, distroless
deploy/kubernetes/     a kustomize base; one replica, and egress denied
docs/frontend-stack.md the front end's dependency and supply-chain policy
docs/eval.md           the defect manifest format and what a score means
docs/scale.md          what happens on two gigabytes, and what it changed
docs/mcp.md            wiring an assistant to `veritix mcp`, and what it may ask
docs/rules-proposal.md propose, review, accept: coverage turned into recall
docs/deployment.md     binary, container, cluster — and what each one promises
LICENSING.md           the dual license: AGPL, or commercial terms
CLA.md                 the contributor agreement that makes the second possible
CONTRIBUTING.md        how to work on it, and the four things a patch must not do
```

`audit.Run` is the single pipeline: discover → engine → ingest → profile →
checks → rules → *lockdown → agent* → verify. The CLI, the HTTP API, and the
MCP server all call it. Three entry points assembling the pipeline slightly
differently is how a tool ends up reporting different results depending on how
it was invoked — and `internal/runs` is the same argument one layer up, for the
bookkeeping that wraps a run rather than the run itself.

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
- **`propose_rule` is the agent's second output, and it inverts the zero rule.**
  `record_finding` says the data is wrong now; `propose_rule` says an
  expectation should hold in future, which is how a defect found on one run
  gets found on every run without a model. Veritix compiles the proposal into a
  real `rules.File`, materializes it, and runs it through the real
  `rules.Evaluate`: same discipline, same disagreement-records-nothing rule for
  the model's stated `violations_now`. But a proposed rule with **zero**
  violations is the best kind of rule, not a claim that failed to reproduce —
  what has to be refused instead is a rule that applies to nothing, which would
  sit in a customer's file forever looking like protection. `one_of` takes no
  value list, because its body is literally cell values: the model proposes the
  shape and `rules.Materialize` fills the permitted set in from the data, in
  the process, for a person to review. The tool result carries the *count* of
  those values and never the values; `TestAProposalsValuesNeverReachTheModel`
  pins it. Nothing is applied. Proposals ride in the same `report.Document` as
  their own section — the *shape* of each rule and a count of what it permits,
  never the values, because a report is a file that gets emailed — and
  `rules.RenderProposals` writes them out as a rules file, which is the one
  place the permitted values are written, since a `one_of` rule without them is
  not a rule. `audit --propose-rules-out` is that file from the command line.
- **Accepting is where a proposal stops being a suggestion.** The store keeps
  proposals whole (the report cannot, since it omits the values), served by
  `GET /runs/{id}/proposals` described and `…/proposals/{pid}` in full;
  `POST /datasets/{id}/rules` writes the accepted rule — with whatever the
  reviewer edited — into `<DataDir>/datasets/<id>/rules.yaml`, which
  `runs.AcceptedRules` loads on *every* later audit of that dataset, over HTTP
  and over MCP alike. `--rules` stays the customer's own file and the two are
  additive; `runs.Merge` refuses a name collision rather than letting one file
  redefine another's rule. `TestAnAcceptedRuleIsEnforcedWithoutTheModel` is the
  milestone in one test: the model proposes on run one, a person strikes out
  the typo, and run two finds the defect with no model at all.
- **The accept screen is where the values are the point.**
  `web/src/components/proposals.tsx` lists a run's proposals out of the same
  `report.Document` the findings come from — one document, so the screen and the
  downloaded report cannot disagree — and fetches `…/proposals/{pid}` only when
  a reviewer presses for one named proposal, which is the boundary the
  offending-rows panel already sits on. What it shows there is the materialized
  vocabulary with a checkbox each, because a set drawn from a column contains
  whatever the column contains: on `dirty-retail` that is `Actve` beside
  `Active`, and accepting it unread enforces the misspelling instead of catching
  it. Name, description and severity are editable, and severity is always sent
  rather than inherited, so a rule that can fail a build does so because
  somebody chose that. A proposal's id is what the rule asserts rather than how
  it was worded, so an accepted rule keeps it and a reload marks the proposal in
  force instead of offering it again and being refused on press.
  `e2e/tests/proposals.spec.ts` drives the whole of it, ending with a second
  audit that runs no model at all. The dataset screen lists what is in force,
  named as the rules file names it — the SQL table name a rule is written
  against, not the source name the proposal screen showed, since that list is a
  view of the file a person can edit.

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
- **DuckDB errors are scrubbed of single-quoted content** — "Could not convert
  string 'N/A' to INT" is a cell value escaping through a diagnostic — **except
  what the model already sent.** `Guard.EngineError` takes the statement and
  passes through any quoted literal appearing in it verbatim, because text the
  model wrote cannot be disclosed by returning it. DuckDB echoes the offending
  statement inside the message, so without that exception every literal in the
  model's own query comes back rewritten: qwen3.5-35b sent
  `REPLACE(amount, ',', '')`, was shown `REPLACE(amount, '⟨,⟩', '⟨⟩')`,
  concluded the engine was mangling its literals and gave up on SQL for the rest
  of a 55-minute run.
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
- **A tool call written as prose is handed back, not executed.** Weak models on
  the chat-completions dialect finish a turn by emitting the call as message
  content; qwen3-4b ended a run with three complete `record_finding` payloads in
  text and nothing recorded. `writtenCall` matches a JSON object in the message
  against the tool *schemas* — every required parameter present, no key that is
  not a parameter — and the loop tells the model, once, to make the call.
  Executing what it wrote is the thing not to do: that would put a finding in a
  report without passing the tool that checks the count against the query. The
  correction says stopping is a legitimate answer, because a nudge that reads as
  "you were supposed to find something" is how a report fills with padding.
- **The trace is a product feature.** It records every payload verbatim on both
  sides, is served at `/runs/{id}/trace`, and is written by `audit --trace-out`.
  It is how a customer checks the egress promise instead of taking it on trust,
  which is why nothing in it is summarized. Both entry points emit the same
  document — the CLI encodes `audit.Result.Trace`, which is what the API stores
  — so there is one answer to "what was the model sent", not two.
- **The transcript carries a rolling cache breakpoint, not just the prompt.**
  A step re-sends every earlier tool call and result, so an agent run's cost is
  quadratic in its length unless the growing prefix is cached. Caching only the
  system prompt is the trap: an 18-step audit read back 3,679 tokens per step,
  flat, and paid full price for 257k. `markConversationPrefix` marks the last
  block of each of the final *two* messages — two because a breakpoint finds an
  earlier entry only by walking back 20 content blocks, and a step appends both
  an assistant message and its tool results, so one mark each halves the reach
  and survives a step that fires many tools at once. Measured on
  `dirty-retail`: full-price input 215k → 42 tokens, a 21-step run costing
  $0.76 against $1.96 for the same work uncached. It changes billing and
  nothing else — caching is a hash over bytes already being sent, so the guard,
  the trace and the report are untouched. `openaicompat` needs none of this;
  llama.cpp and Ollama reuse their own KV cache.
- **A model that misbehaves is not an error.** Bad arguments, refused SQL, a
  finding that does not reproduce — all come back to the model as tool errors so
  it can correct itself. A run ends when the model stops or a budget does.
- **An identical refused call is named as one**, because a model will send it
  again. gpt-oss-120b sent the same `propose_rule` four times running on
  `dirty-logistics` — a `one_of` naming no column — and got the same correct
  refusal each time; at five minutes a step against a budget of 24 that is a
  sixth of the run. Nothing in the refusal distinguished "you got this wrong"
  from "you got this wrong in exactly the same way you just did", which is the
  distinction that would have made it change something. `Registry.noteRepeat`
  counts each call by its canonicalized arguments and appends the attempt
  number, what to change, and that moving on is legitimate — the same last
  clause `writtenCallCorrection` carries, for the same reason. It is a note and
  not a stop: the budget is still the backstop and what to do next is still the
  model's. That run also produced the other half of the fix — every message out
  of `rules.Validate` now says what to change rather than only what is wrong.

The honest limit, stated in `redact`'s doc comment: the guard bounds what
Veritix *sends*. It is not a defense against a model deliberately smuggling data
out through carefully chosen aggregates. The guarantee is that ordinary
operation discloses no cell values, and that everything sent is in the trace.

## How telemetry is put together

`internal/telemetry` owns slog and, since M6b, OpenTelemetry. `docs/deployment.md`
is the operator's half; these are the decisions.

- **Off by default, for the reason the model provider is.** An OTLP endpoint is
  a network egress from a process holding data the customer declined to send to
  a vendor. `otel.enabled` is `false`, and a build that started exporting
  because an ambient `OTEL_EXPORTER_OTLP_ENDPOINT` happened to be set would be
  Veritix making that call on their behalf. Once it is on, the standard
  `OTEL_EXPORTER_OTLP_*` variables are honored by the exporters, so an
  operator's existing collector configuration works: enabling is Veritix's
  switch, where to send is theirs.
- **A span carries counts, never names.** Stage names, tool names, severities,
  origins, provider and model identifiers, token counts, durations, route
  patterns. **Never** a table name, a column name, a file path, SQL text, model
  prose, or a cell value. A span is an access log that leaves the machine, and
  the schema of a customer's export is itself commercially sensitive — this is
  `finding.Finding.ID`'s argument (ids end up in URLs and access logs) one step
  further out. The half that is easy to lose is the schema half: nobody would
  put a cell value in an attribute on purpose, and putting the table name in
  one is the obvious, helpful, wrong thing to do.
- **`TestNoSpanCarriesCustomerData` is what holds that**, not the comment. It
  audits `dirty-retail` with a recording exporter installed and a scripted
  model driving the real agent loop, then scans every exported span — name,
  status, attributes, events, resource — for the fixture's cell values *and*
  for its file and column names. Same shape as the report and MCP egress tests,
  and for the same reason: the promise is about what left, so the test looks at
  what left.
- **The HTTP span is named after the route pattern, not the path.** A path
  carries a dataset id, a run id, a finding id. `r.Pattern` is only known after
  routing, so the span is renamed on the way out. It also keeps span names to a
  fixed set, which is what a collector wants anyway.
- **The providers are global, which is the one place this repo does not
  inject.** OpenTelemetry's global is a delegating no-op until something sets
  it, so an unconfigured build pays an interface call per span and `audit.Run`
  does not grow a parameter that exists only for observability. Instruments are
  built once against the global meter and delegate the same way. The test
  installs its own provider exactly as `Start` does.
- **`Shutdown` carries its own timeout.** It runs while the process is exiting,
  often because somebody pressed Ctrl-C, so the caller's context is already
  done. It is called from `Execute` after `ExecuteContext` returns rather than
  from a cobra `PersistentPostRun`, because that hook does not run when a
  command returns an error — and a failed audit is exactly the run whose
  telemetry somebody wants.
- **The endpoint is a base URL**, `http://collector:4318`, because that is what
  `OTEL_EXPORTER_OTLP_ENDPOINT` means and what an operator will paste in.
  `signalURL` appends `/v1/traces` and `/v1/metrics`; a URL that already names a
  path is left alone. The exporter's own `WithEndpointURL` wants the full
  signal URL and posts to `/` without this, which is a configuration that looks
  right and sends nowhere — `TestEnablingActuallyExports` is there because that
  failure survives every review.
- **The cost, measured before adopting it:** 17 new modules and +4.7 MB on an
  87 MB binary. OTLP-over-HTTP still links gRPC, because
  `go.opentelemetry.io/proto/otlp` carries the collector's gRPC service
  definition alongside the message types. All Apache-2.0 or BSD-3-Clause, so
  nothing here is a term the commercial license could not deliver.

## How it is deployed

`deploy/` is the shipping half of M6b and `docs/deployment.md` is the argument.

- **The image builds the interface or fails.** `deploy/Dockerfile` gained a
  Node stage: plain `go build` produces a working API and a page saying the
  interface is missing, which is right for a developer and wrong for an image
  somebody deploys. `veritix version --json` reports `"web": true`, and the
  build asserts on it rather than shipping a blank page.
- **One replica, and it is a constraint rather than a starting point.** A run
  keeps its DuckDB file on the pod's volume so the rows endpoint can reopen it,
  and the SQLite store beside it is the audit trail. A second replica serves a
  different history and answers a rows request from the pod that does not have
  the file. Hence `replicas: 1`, `strategy: Recreate`, `ReadWriteOnce` — which
  agrees with the licensing shape and the data-locality shape, so it is a
  constraint that agrees with itself.
- **Egress is denied by default, as a cluster object.** The egress guard bounds
  what the *agent* sends and the trace records it; a NetworkPolicy bounds what
  the process can reach at all. One is a design a reviewer has to read, the
  other is a control an auditor can check, and the product's whole proposition
  is worth stating both ways. A model endpoint inside the cluster and an OTLP
  collector are the two commented exceptions.
- **`VERITIX_CONFIG` names the config file**, because a container's config
  arrives on a mounted volume at a path the image did not choose, and keeping
  it out of the command line means overriding `args` does not silently lose it.
  A named file that does not exist is an error, exactly as `--config` is.
- **`engine.memory_limit` must stay below the container's limit.** DuckDB
  spills to disk at its own limit and is OOMKilled at the cgroup's: the first
  costs time, the second costs the run. The base ships 2GB against 3Gi.
  `readOnlyRootFilesystem` is why `engine.temp_dir` points at an `emptyDir`.
- **The image is built locally with Podman, and the Dockerfile has to stay
  plain for that to keep working.** Podman is rootless and daemonless, so
  building an image here costs nobody a `docker` group that is root in all but
  name; `podman-docker` supplies a `docker` shim, so `make docker` is unchanged
  and tags from `git describe` as usual. What buys that is the Dockerfile using
  no BuildKit-only syntax — no `# syntax=`, no `--mount`, no `--link`, no
  heredoc `RUN`. **Adding one would not fail; it would make the local build
  stop matching the CI build**, which is the worse outcome, so add a BuildKit
  feature only along with a decision about which builder is the authority.
- **What the first real build settled, so nobody re-derives it:** the image is
  118 MB; `corepack pnpm install --frozen-lockfile` works in a clean container;
  `COPY --from=web` lands before the Go build, so `//go:embed all:dist` gets a
  real bundle; distroless `cc-debian12` carries enough C runtime for the
  CGO/DuckDB binary; the `"web": true` assertion fires; the container refuses
  `serve` without a token; `/health` answers unauthenticated while
  `/api/v1/runs` is 401; the interface is served behind its full CSP and a
  client-side route survives a reload; and `--read-only` works, which is
  `readOnlyRootFilesystem` validated for real.
- **The `/tmp` volume is no longer decorative, and its size limit is now load
  bearing.** It used to be for DuckDB's spill alone, which neither fixture is
  big enough to trigger; a run given no database path now puts its DuckDB file
  there too, at roughly a third of the dataset's CSV size. The base's
  `sizeLimit: 8Gi` therefore caps what a CLI audit *inside the container* can
  load, on top of whatever the spill wants. A run started over HTTP or MCP is
  unaffected — those put the file on `/data`, where it belongs, because the
  rows endpoint reopens it. What is still untested is the spill itself.

## How the MCP server is put together

`internal/mcp` is a third door onto the same building, not a third building.
`veritix mcp` serves stdio; `docs/mcp.md` is how to wire an assistant to it.

- **An MCP-started audit is an ordinary run.** It goes through `runs.Execute`
  and therefore `audit.Run`, is recorded in the same SQLite store, keeps its
  DuckDB file in the same place, and produces the same `report.Document`. Point
  `veritix mcp --data-dir` at the directory `veritix serve` uses and an audit an
  assistant ran is in the run list in the browser, rows and all.
- **`internal/runs` exists so that is true by construction.** It holds the
  bookkeeping that wraps a run — StartRun, `report.Build`, close the engine,
  FinishRun, SaveTrace — and the order is load-bearing: the engine is released
  before the run is recorded as finished because the DuckDB file has to be
  flushed before the rows endpoint reopens it. Two callers each remembering
  that for themselves is how one of them eventually forgets. `internal/api`
  keeps the SSE fan-out, which is its own business, and calls `runs.Execute`
  through a `Watch` callback.
- **The caller chooses what to audit; the operator chooses what Veritix may
  disclose.** `--include-values` and `--agent` are flags on the server, not tool
  parameters, because the client of an MCP server is somebody else's model in a
  context Veritix neither controls nor records. Lifting an egress policy is a
  decision a person takes. `TestNoToolLetsTheCallerLiftTheEgressPolicy` pins
  that no tool schema carries one.
- **There is no rows tool.** The per-finding rows endpoint is `internal/api`'s
  one deliberate exception and stays there: over HTTP it is one person clicking
  one finding in a page they opened, and an automated caller could walk every
  finding of every run. Same data, different thing.
- **Everything served is read back from the stored document**, decoded rather
  than rebuilt, for the same reason the HTTP API writes those bytes verbatim.
  One document, built once, so what an assistant is told and what a person sees
  cannot drift.
- **`audit_dataset` is synchronous, on the caller's context.** An assistant
  asked a question and is waiting; a tool returning an id to poll would spend
  the caller's turns on bookkeeping. Cancellation follows the call.
- **A caller's mistake is a tool error, not a protocol failure** — the same rule
  `tools.Registry.Invoke` follows for Veritix's own agent. An unknown id, both
  `path` and `dataset_id`, a severity that is not one: all come back as
  something the model can correct.
- **It does not call `store.MarkInterrupted`.** `api.New` does, because a run
  marked in-flight belongs to the process that started it. An MCP server is a
  subprocess that may be one of several, possibly alongside a `veritix serve`
  with runs genuinely running, and marking those interrupted would be one
  process declaring another one's work dead.
- **The trace promise does not extend past the boundary.** `/runs/{id}/trace`
  records what Veritix's *own* agent was sent. It says nothing about an MCP
  client, because Veritix is not driving that model. `docs/mcp.md` says so
  rather than letting the existing claim quietly over-reach.

## How the eval is put together

`internal/eval` scores an audit against a dataset whose defects are already
known. `docs/eval.md` is the whole of it; these are the decisions.

- **Two numbers, and they do not collapse into one.** Mean recall is what one
  audit finds; coverage is what repeated runs find between them. Half and half
  is a model that finds some defects and misses others; half and all is a model
  that finds a different one each time, which is what three `gpt-oss-120b` runs
  on `dirty-retail` turned out to be doing. A single figure would have called
  both of those the same product.
- **Credit is the engine's number at the manifest's location, never the
  model's prose.** A model's rule slug and title are wording, and two runs word
  the same defect two ways; scoring on them would measure vocabulary. Location
  alone would credit any observation about the column, and a count alone would
  credit a coincidence elsewhere, so it takes both. `MatchesTarget` is the only
  definition of "found it", because `internal/checks`'s suite asks the same
  question for the opposite reason — has a deterministic rule started covering a
  target the model is still being paid for.
- **The manifest's own counts are re-run**, from `agent.query`, exactly as a
  finding's evidence is. A target with a wrong count is a target nothing can
  ever match, and the eval would report every model missing it forever with
  nothing saying why.
- **Row counts and distinct counts have to agree.** Veritix's own
  `reference.orphan_values` counts distinct offending values; a model writing
  `count(*)` counts rows. Where they disagree one of two correct models is
  refused credit and it reads as the model's failure.
  `TestAgentTargetCountsDoNotDependOnPhrasing` pins it. `equivalent:` exists for
  targets that genuinely admit two figures and should stay rare: reaching for it
  once immediately collided with a `column.missing_values` finding measuring the
  same number at the same location, and `sales.xlsx` was edited instead.
- **An eval run is an ordinary run.** `eval.Run` drives `audit.Run`, so what is
  scored is the auditor a customer runs. One thing is forced rather than
  configured: `--include-values` is off whatever the configuration says, because
  a score obtained by showing the model cell values is not a score for the
  product anybody ships. `TestEvalWillNotShowTheModelCellValues` pins it.
- **The gate fails on the checks and not on the model.** A missed planted defect
  or a check firing on clean data exits non-zero unasked, because the manifest
  is not an opinion. `--min-recall` is opt-in, because a build that fails when a
  model has a bad afternoon is a build people learn to ignore.
- **Measured, on `dirty-retail` with `qwen3:4b-instruct-2507`:** mean recall
  17%, coverage 50% over three 14-step runs. The two figures come apart exactly
  as designed — one run of that model reports 0% or 50% depending which one you
  take. What separated the run that scored from the two that did not was six
  tool calls spent on `describe_table` instead of `check_referential_integrity`;
  one of the failures ended its budget one corrected call short of the finding
  the success recorded. `docs/local-model.md` has the traces. A 24-step attempt
  on the same machine could not finish a run at all, which is the opposite of
  the lesson the 120b taught.
- **Two fixtures measuring different things.** `dirty-retail`'s targets are both
  unresolved references, so it measures whether a model will use a tool surface
  it was not asked to use. `dirty-logistics`'s four are invisible to every check
  tool — a row whose two dates contradict each other, three weights in grams in
  a kilogram column, a currency column contradicting the name of the amount
  column beside it, and a contradiction that only exists across a join. A model
  can score full marks on the first with four tool calls and zero on the second.
- **Measured, on `dirty-logistics` with `gpt-oss-120b`:** mean recall 42%,
  coverage 75% over three runs, checks 9 of 9 with no false positives. The four
  targets land at four different rates (3/3, 1/3, 1/3, 0/3), which is what a
  fixture with more than one target in reach at once buys. **No run was stopped
  by a budget** — all three finished voluntarily at 10-12 steps of 24 — so on
  this model the ceiling is the stop decision, not the step count, which is the
  opposite of every earlier local measurement here. What separated the 3/4 run
  from the two 1/4 runs was one tool call: both losers opened with
  `sample_values` on an enum column, read the differing shape lengths as a
  formatting defect, and spent three steps proving something true that is not a
  defect. `docs/local-model.md` has the call sequences.
- **A conversion is a stronger claim than a recall credit, so it is held to a
  stricter match.** `MatchesTarget` lets a table-scoped finding cover any column
  in that table, because a model writes prose and scoring it strictly would
  measure phrasing. `convertedBy` refuses that: it wants the manifest's exact
  location. Found by a real proposal — gpt-oss-120b proposed an `expect: sql`
  rule on `shipments_csv` catching 2 of the 3 grams-in-kilograms rows, and
  scored loosely it credited `delivered_before_dispatch`, a different defect in
  a different pair of columns that also affects 2 rows. Both halves wrong, and
  the scorecard said the loop had worked. A rule that means to protect a column
  can say so, and `rules.Evaluate` now carries an `sql` rule's `column` into its
  finding's location — which is also where a person looks for it in the report.
- **`--rules` is how the rule-proposal loop is measured rather than asserted.**
  `propose_rule` exists to convert coverage into recall, and until M6b the
  instrument built to read exactly that could not see it: `ScoreRun` credits
  only agent-origin findings, so an accepted rule catching a target scored
  zero. `ChecksScore.Converted` is that target moved out of `Uncovered` — the
  one figure on the scorecard that shows the return on paying a model, once per
  class of defect rather than once per audit. It is reported apart from recall
  on purpose: mean recall stays agent-origin only, because folding in a rule a
  person accepted last month would make a model look better every time a human
  did some work. `MatchesTarget` is still the only definition of "found it".
  `TestAnAcceptedRuleConvertsAnAgentTarget` pins it on `dirty-logistics` with
  no model configured at all.
- **`noise:` is the manifest's answer to a claim somebody has already
  adjudicated.** `clean:` polices the checks, whose rule names Veritix chose; it
  cannot police an agent claim, because the rule slug is the half the model
  writes and two runs worded the same observation two ways. A noise entry is
  keyed the way a target is keyed — the engine's number at a location — and it
  labels rather than penalizes: marking a model down for noticing something true
  would grade its judgment through its wording. `Validate` refuses one that
  measures a target's count at a target's location.

## How it behaves at size

Both committed fixtures are tens of rows, which is where every threshold in
`internal/checks` looks fine and where nothing about cost is visible.
`scripts/gen-dataset` writes one that is not: 2 GB, 22M rows, 231 columns,
seeded so a measurement can be repeated, and carrying a
`veritix-manifest.yaml` written from the generator's own tallies so
`veritix eval` scores the same run that is being timed. A scale test that only
measures seconds cannot tell a fast auditor from one that quietly stopped
looking. `docs/scale.md` has the numbers; these are the decisions.

- **A defect is the same size and the file is not, so a check must not measure
  a share.** `column.missing_values` ignored a column under 5% missing: `N/A`
  in two of nine rows is 22% and fires, thirteen of two million is 0.0006% and
  did not. A placeholder defeats every null check downstream whatever its
  share, and the bigger the file the less likely anybody notices by eye. Text
  placeholders now count at any rate; genuine blanks keep the 5% floor, because
  a column that is 3% empty really is unremarkable. A *numeric* placeholder is
  the hard half — `-1` and `999` are sometimes measurements — so it counts only
  where it stands out from the column around it: more repeated than any real
  value (`standsOut`), or negative where nothing real is (`wrongSideOfZero`,
  against `NumericStats.MinReal`, which excludes the magic numbers from their
  own comparison). Sign rather than distance, because "just past the maximum"
  is where the largest value of every uniform column lives.
- **`column.mixed_date_formats` had the right reason and the wrong test.** It
  dismissed a format under 2% of the column as one format matched by two
  patterns — `05/06/2019` parses day-first and month-first alike — which is
  real, but 0.1% is also what a genuine second format looks like in a
  two-million-row export. `FormatCount.Exclusive` measures it directly: how
  many values a format reads that the *leading* format cannot. A format
  explaining nothing new is not a second format at any size. The leading
  format's probe goes first in the SQL so DuckDB's short circuit evaluates the
  rest only on rows that failed it, which is what keeps the extra pass to about
  one `strptime` per row.
- **A measurement that did not run is a finding, not a log line.** At 20M rows
  every column of the biggest table exceeded the old two-minute query timeout;
  `profile.Run` logged a warning, substituted a stub, and the audit reported
  13 of 17 planted defects with **zero false positives** — a confident report
  on a table nothing had looked at, because a stub has no nulls, no
  placeholders and no type violations. `column.not_profiled` says so, carrying
  no evidence query (the measurement is what failed) and no engine error text
  (a DuckDB error quotes the value that caused it). No other column check runs
  on such a column, and `table.no_candidate_key` goes quiet when any column in
  the table is unmeasured — "no column identifies a row" is a claim about every
  column, and it reads as a defect in the data rather than a gap in the audit.
- **Two query timeouts, because they bound two different things.**
  `engine.query_timeout` is one of Veritix's own measurements over a whole
  column and defaults to 30 minutes: it has to be sized for the dataset the
  product exists to audit, and a limit below what a column costs does not fail
  the audit, it drops the column. `engine.agent_query_timeout` defaults to two
  minutes and bounds a statement the model wrote, which is where "one runaway
  query must not exhaust the host" always belonged — that SQL is unreviewed,
  arrives up to forty times a run, and is the only SQL in the process nobody
  chose. `tools.Registry.Invoke` applies it once for every tool rather than
  each tool applying it, and `agent.Options.UseEngineLimits` carries both from
  config, because four entry points were each copying `MaxRows` by hand.
- **A finding counts what its own evidence matches.** `column.missing_values`
  counted every sentinel and demonstrated itself with a query matching only the
  textual ones. `Set.Verify` trusts the engine and *silently corrects* a
  disagreeing count, so the title kept one number while the finding carried
  another — the exact failure `record_finding` refuses to allow the model, in a
  deterministic check. Build the predicate from the values actually counted.
- **Profiling is the cost and it is linear in cells, not rows.** Ingest is 7%
  of a run: DuckDB reads 2 GB of CSV and writes its own storage in a minute.
  A cell then costs about 5 µs — four to six full scans with regular
  expressions and date parsing on every value — so 201 columns of 200k rows
  cost three and a half times what 2M rows of twelve columns did, and were the
  one table that got no faster when the tables moved into a file. Parallelism
  is eight columns at a time and is not configurable. The whole 2 GB run is
  14m 30s and 4.2 GiB resident.
- **Every run holds its tables in a DuckDB file, and that is not a trade.**
  The HTTP API and the MCP server always passed a `DatabasePath`, because the
  rows endpoint reopens the file afterwards; the CLI held its tables in memory,
  which reads like memory bought speed. Measured on 400 MB it bought neither:
  DuckDB's persistent storage is compressed where its in-memory tables are not,
  so the file is a third the size of the CSV and the scan that reads it
  finishes sooner. `audit.Run` with no `DatabasePath` now takes a temporary
  directory and `Result.Close` removes it — the flag still means "keep the
  file", and nothing else changed. The cost is scratch space of about a third
  of the dataset, in `engine.temp_dir` when one is set and the system temp
  directory otherwise — which on many Linux boxes is a tmpfs, and those pages
  are RAM. Measured at 400 MB that is the difference between 1.06 and 1.15 GiB
  resident, so it costs the size of the file rather than the benefit; on
  anything large, point the setting somewhere real.
- **A stage now announces itself on entry and reports its duration on exit.**
  Ten and a half minutes passed between "loaded dataset" and the next line,
  and the browser's progress stream is those same log lines. `profile.Run`
  logs each table as it finishes, which is also what makes the measurements
  takeable.

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
accident, which is not a reason to make it tedious. It runs **`gpt-oss-120b`
under llama.cpp by default**, since that is the only model measured here that
does the interesting half of the job, and it **starts the server itself** —
`~/big-local-llms/scripts/serve-prefetch.sh`, stopped again at exit — when
nothing answers `BASE_URL`. One that is already up is left alone and left
running, which is how to keep a model warm across runs. A small model is an
override away (`BASE_URL=…:11434/v1 MODEL=qwen3:4b-instruct-2507-q4_K_M
EFFORT=none TIMEOUT=30m`) and is still the cheap way to exercise a change to the
loop rather than to the auditing.
- Ollama sizes its context window from VRAM and picks **4096 tokens** when there
  is no GPU. Veritix's first agent prompt is ~4080 since the profile moved into
  the brief, so it does not fit at all; even when it did, llama.cpp discarded
  from the front within a step or two — taking the system prompt with it. The
  model stops knowing it may not see cell values and starts answering in prose,
  which reads as a stupid model rather than a truncated context. `OLLAMA_CONTEXT_LENGTH=32768` before `ollama serve`, always.
- Take a **non-thinking** model (Qwen3's `2507` instruct tags). A hybrid emits a
  reasoning block before every tool call, which on a CPU costs the same per
  token as useful output, and `openaicompat` drops it on the way back anyway.
  Where there is no such tag — every `qwen3.5` variant is hybrid — `llm.effort`
  (`--llm-effort none`) goes out as `reasoning_effort` and Ollama honors it:
  73 completion tokens for one tool call becomes 14.
- Probe `/v1/chat/completions` with a two-tool payload before running a full
  audit: twenty seconds to learn what a full run takes twenty minutes to prove.
- **Ollama is `openaicompat`'s default base URL because customers have it, not
  because it is faster** — that is the product's default, and it is not what the
  test script points at any more, which is llama.cpp on 11500.
  llama.cpp's `llama-server --jinja` has been run end to end against
  `dirty-retail` and is the same speed — identical 94 s median step, 24 tool
  calls, none refused, byte-identical deterministic report. `--jinja` is what
  makes it call tools at all, and it reads Ollama's blobs directly since those
  are plain GGUF. The preflight in `scripts/local-model.sh` detects which server
  answered and adapts; where it can read neither `/api/ps` nor `/props` it now
  *says so*, because a silently skipped context check is indistinguishable from
  a passing one. `docs/local-model.md` has the measurements.
- **`reasoning_effort` has two spellings and the servers disagree.** Ollama
  reads the top-level field; llama.cpp hands the request to the model's own
  jinja template, and gpt-oss's harmony template reads it only out of
  `chat_template_kwargs`. Neither errors on the one it ignores. Sent the wrong
  way it is silently inert — `low` and `high` measured 285 and 243 completion
  tokens, against 47 the right way — and a 24-step audit spent 6h47m reasoning
  at an effort nobody asked for. `openaicompat` sends **both**;
  `TestEffortIsSentBothWays` pins it. `scripts/local-model.sh`'s probe is raw
  curl and needed the same fix, plus `PROBE_TIMEOUT` (default 900s), since it
  was failing models in preflight that a run handled fine.
- **A model larger than RAM needs llama.cpp, and three flags.** Not Ollama: a
  model that does not fit fails on ggml's SIMD repack buffer, which materializes
  weights in anonymous memory and so defeats mmap by construction — every quant
  type has repack traits, `mxfp4` included, so no choice of quantization avoids
  it, and Ollama exposes no switch. `llama-server --no-repack --fit off
  --load-mode mmap` is the recipe; `--fit off` matters as much as the others,
  because auto-fit otherwise falls back to a non-mmap allocation of the whole
  file and fails with a different message for the same cause. Ollama's *bundled*
  llama-server has the flags. Confirm it took by `VmSize` ≫ `VmRSS`. Ollama's
  own qwen3.5 GGUF cannot be paged at all whatever the flags — it transforms
  tensors at load, which disables mmap, and upstream llama.cpp rejects the file
  outright. `gpt-oss-120b` (63GB, 5.1B active) runs this way at 0.63 tok/s
  generating and 4.4 tok/s prefilling a long prompt. Active parameters, not
  total, set the paging bill. `docs/local-model.md` has the measurements.
- **`--ubatch-size` is the flag that makes a too-big model usable, and 512 is
  the wrong default for one.** Prefill reads each expert once per micro-batch
  and uses it for every token in that batch, so the micro-batch size divides the
  I/O outright. Raising it to 2048 took the ~6300-token brief's first step from
  2308s to 1522s — **1.7x**, and the difference between a 24-step audit that
  ends on `provider_error` mid-prefill and one that finishes in 59 minutes with
  two verified findings. Serve with `scripts/serve-prefetch.sh` in
  `~/big-local-llms`, which sets this along with the three paging flags, an
  expert-prefetch `LD_PRELOAD` hook, and `--parallel 1` — several slots let a
  follow-up turn land on a cold one and re-prefill the whole conversation, which
  here is half an hour. The **hook itself is neutral for an agent** and was
  measured to be: prefetch pays only while what it advises survives in RAM, and
  a prefill micro-batch selects nearly the whole expert pool, so it advises 317GB
  into 30GB and re-reads what was evicted. It now advises for generation only,
  where it is worth 1.3x — which is 1% of a call that is 99% prefill. The paging
  flags and `--ubatch-size` are what make the model usable. **`--llm-effort none` is silently ignored by gpt-oss**:
  harmony knows `low`/`medium`/`high` and quietly defaults anything else, at 317
  output tokens against 132, which reads as a slow model rather than a setting
  that did not take. Size the request timeout above the
  *first* step, which is nearly half the wall clock. There is no flag for it:
  it is `llm.request_timeout` in the config, or `VERITIX_LLM_REQUEST_TIMEOUT`,
  which is what `scripts/local-model.sh` sets from `TIMEOUT`.
- **Parameter count does not predict whether a model can do this job.**
  `qwen3:4b-instruct-2507` uses the check tools and records the finding
  `relate.go` misses; `qwen3.5:35b-a3b` is eight times the size and answered
  three whole runs with nothing but `run_sql` — forty calls, no
  `record_finding`, findings narrated in prose at the end. What separates them
  is whether the model will use a tool surface it was not asked to use, and the
  only way to know is to run one audit and read the trace.
- **A clean `check_referential_integrity` is evidence about that pair, not about
  that column**, and a model reads it as the latter. gpt-oss-120b has found each
  of `dirty-retail`'s two unresolved references on a different run and never
  both: on 16 Aug it checked `sales_xlsx_q1.region` against the workbook's own
  `sales.xlsx#reference`, where every code resolves, called the column fine and
  moved on — the 2 orphans are against `regions.csv`, which is the parent the
  previous run happened to try first. Neither run measured anything wrong. Worth
  knowing before reading a single run as coverage, and worth a second dataset
  before deciding whether the tool result should say so; the fixture cannot tell
  "found the defects" from "found *a* defect and stopped", since there is one
  reachable per column. `docs/local-model.md` has both traces.
- A small model does not ration its step budget — the first run here spent six
  consecutive steps on `describe_table` and finished with nothing recorded, and
  two more 12-step runs did the same. Budget for the model, not the dataset:
  the script defaults to 24 steps and a 60-minute per-call timeout, since a
  longer run reaches the slow full-context steps that outrun the product's
  10-minute default and would otherwise end on `provider_error` — and with a
  paged model it is the *first* step that has to fit inside it.

**OpenTelemetry**
- **`resource.Merge` refuses two different semconv schema versions.** The
  version imported has to be the one `resource.Default()` was built with, or
  `Start` fails at startup with "conflicting Schema URL" — bump it with the
  SDK. Caught by `TestEnablingActuallyExports`, which is the only thing that
  would have caught it: the code reads correctly either way.
- **`WithEndpointURL` wants the full signal URL, not the base.** Given
  `http://collector:4318` it posts to `/`, and a collector answering 200 to
  everything makes that look like it worked. `otel.endpoint` is a base, because
  that is what `OTEL_EXPORTER_OTLP_ENDPOINT` means; `signalURL` appends
  `/v1/traces` and `/v1/metrics`.
- Cobra's `PersistentPostRun` **does not run when a command returns an error**,
  so a flush hung on it loses the telemetry of exactly the runs somebody wants.
  It is called from `Execute` after `ExecuteContext` returns instead.

**MCP**
- **Raw JSON-RPC piped in from the shell does not smoke-test it.** The pipe
  reaches EOF while the audit is still running, the session is torn down, and
  *nothing* is written back — not even the `initialize` response that was
  already answered. It looks like a server that ignores its input. Connect a
  real client as a subprocess instead (`mcp.CommandTransport`); that is also the
  only way to catch a stray write to stdout, which is the protocol.
- **Do not illustrate the egress policy with a value from the fixtures.** The
  server's instructions explained shapes with "CUS-000001 is reported as
  XXX-999999", and `TestNothingSentOverMCPContainsARawValue` flagged it —
  correctly, because a scanner cannot tell an example from a leak and neither
  can a reader watching bytes cross the wire. Describe the shape instead of
  inventing an instance of it. Prose in `docs/` is free to use the example; text
  the server *sends* is not.

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

`testdata/dirty-retail/` and `testdata/dirty-logistics/` carry deliberately
broken files, and each one's `veritix-manifest.yaml` is the list: every planted
defect with the check that must catch it, a companion list of places the data is
clean that must stay quiet — a check that fires on everything is useless — and a
`noise:` list of true observations that are not defects, so a claim a person has
already ruled on is labeled rather than re-adjudicated every run. The manifest is one file, read by `internal/eval`'s tests and by
`veritix eval` alike; a second copy of a defect list disagrees with the first
eventually, and then a passing test means nothing. Add to both halves when
adding a check, and add a new fixture to `scoredFixtures` in `eval_test.go`,
which is the only wiring a dataset needs.

`sales.xlsx` is a committed binary fixture (title rows, a hidden row, merged
cells, `#REF!`/`#DIV/0!`, a stacked TOTAL table, a hidden sheet). It was
generated by a throwaway program; regenerate by hand if it ever needs changing.

`internal/checks/scale_test.go` builds a single column two hundred thousand
rows deep out of DuckDB's own `range()`, so it costs a few seconds per case
rather than the minutes a file of that size would, and says what a
two-million-row export would say. It exists because the committed fixtures
cannot answer the question that matters for a threshold — a defect is the same
size and the file is not — and it pins both directions: the rare placeholder
that must still be found, and the incidental 999999 in a column of unique ids
that must not be. `scripts/gen-dataset` is the same question at 2 GB, by hand.

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

`internal/mcp`'s tests drive the real server over an in-memory transport with a
real SDK client, against the same fixtures — no stub pipeline, for the reason
`internal/api`'s tests give. Every frame in both directions is recorded through
`mcp.LoggingTransport`, so the egress test scans the bytes that actually
crossed the connection rather than the values a handler meant to return.

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
screens can be driven without a network model. **Which reply it sends is read
out of the transcript**, not counted on the server: a counter is state shared
between audits, and the second spec to run one would start at the end of the
script and be told the model had finished before it had done anything — which
presents as an agent that produced nothing rather than as a stub that ran out.
Every reply the model has already given is in the request, so each one decides
for itself. `scripts/local-model.sh` is the
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
- **Podman, not Docker, builds the image here.** `sudo apt-get install -y
  podman podman-docker` on Ubuntu 26.04 — both are in the distribution's own
  repositories, so there is no third-party apt source or signing key in the
  chain, which is the same test every other dependency in this project has to
  pass. It runs rootless with no daemon. `make docker` works through the shim.
  See "How it is deployed" for the constraint that keeps it faithful to CI.
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
- **The MCP Go SDK is the one dependency M5 added** — the official
  `github.com/modelcontextprotocol/go-sdk`, seven modules with its transitive
  set, Apache-2.0 and permissive throughout, reasoning in `docs/mcp.md`. Unlike
  the OpenAI-compatible provider, MCP is a versioned specification with a
  reference Go implementation maintained beside it, so hand-writing the framing
  and the schema inference would be a standing cost against a moving target for
  no gain in auditability.
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
