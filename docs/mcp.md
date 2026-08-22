# Veritix over MCP

`veritix mcp` serves Veritix's audits to an assistant — Claude Code, Claude
Desktop, anything speaking the Model Context Protocol — over stdin and stdout.
Ask the assistant to audit a folder, and it calls the same pipeline the web
interface drives.

MCP goes both ways here, and the two halves are separate features that happen
to share a protocol. **Server mode** is this: somebody else's assistant asks
Veritix to audit something. **Client mode** is the other direction — Veritix's
own agent reading the customer's documents so that it knows what their columns
mean — and it is [its own section below](#client-mode-the-customers-own-documents).

## Wiring it up

The server is a subprocess launched by the assistant, so what it needs is a
command rather than a URL.

Claude Code:

```sh
claude mcp add veritix -- /path/to/veritix mcp --data-dir ~/.veritix
```

Claude Desktop, in `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "veritix": {
      "command": "/path/to/veritix",
      "args": ["mcp", "--data-dir", "/home/you/.veritix"]
    }
  }
}
```

Point `--data-dir` at the same directory `veritix serve` uses and the two share
a history: an audit an assistant ran is in the run list in the browser, with
its findings, its report, and its offending rows. That is the intended setup.

## The tools

| Tool | What it does |
|---|---|
| `audit_dataset` | Audit a path (or a registered dataset) and return what was found. The workhorse. |
| `register_dataset` | Name a path in advance. Not needed: `audit_dataset` takes a path directly. |
| `list_datasets` | What this instance knows about. |
| `list_runs` | Past audits, most recent first. |
| `get_run` | One audit's status and counts. |
| `list_findings` | An audit's findings, filterable by severity and pageable. |
| `get_report` | The full report: findings plus the profile of every table and column. |

`audit_dataset` is synchronous. An assistant asked a question and is waiting
for the answer, and a tool that returned an id to poll would spend the caller's
turns on bookkeeping. Cancellation follows the call, so a client that gives up
stops the audit.

Everything is read back from the stored `report.Document` — the same one the
JSON report writes and the web interface displays — so what an assistant is
told and what a person sees cannot drift apart.

## What the caller may decide, and what it may not

**The caller chooses what to audit. The operator chooses what Veritix may
disclose.**

The client of an MCP server is somebody else's model, running in a context
Veritix neither controls nor records. So the two decisions that matter are
flags on the server, not tool parameters:

- `--include-values` permits verbatim cell values in what this server returns.
  Off by default, like every other entry point. Without it a column is
  described by the shape its contents take.
- `--agent` runs Veritix's own model-driven pass on every audit. Off by
  default. It spends the operator's tokens and sends dataset metadata to the
  operator's provider, so it is not something a caller should be able to switch
  on for itself.

`TestNoToolLetsTheCallerLiftTheEgressPolicy` pins this: no tool schema may
carry an `include_values`, `allow_sample_values`, or `agent` parameter.

There is also **no tool for a finding's offending rows**. Over HTTP that
endpoint exists and is deliberate — showing somebody the three bad rows is the
most useful thing the interface can do, one finding at a time, in a page they
opened themselves. An automated caller could walk every finding of every run,
which is a different thing wearing the same name. The exception stays in
`internal/api`.

The limit is worth stating plainly, as `internal/agent/redact` states its own:
this bounds what Veritix *sends*. Once a response reaches the assistant it is
in a context Veritix cannot see, and there is no trace of what happens to it
there. The `/runs/{id}/trace` endpoint records what Veritix's *own* agent was
sent; it says nothing about an MCP client, because Veritix is not the one
driving that model.

## Trying it without an assistant

The tests drive the real server over an in-memory transport
(`internal/mcp/mcp_test.go`), which covers the tools and the egress promise
without a client. To check the binary itself — the stdio framing, and that
nothing has started writing to stdout, which is the protocol — connect a real
MCP client to it as a subprocess. Raw JSON-RPC piped in from the shell will not
do: the pipe reaches EOF while the audit is still running and the session is
torn down before anything is written back.

```
run 01a01c30-5492-7452-aaec-80e96cfb633b succeeded map[errors:14 info:9 total:36 warnings:13]
note: Cell values are withheld: a column is described by the shape its contents take, such as XXX-999999.
  error    table.duplicate_rows         customers.csv has 1 fully duplicated row(s)
  error    key.duplicate_values         customer_id repeats 1 value(s) despite looking like an identifier
  error    column.mixed_date_formats    signup_date mixes 2 date formats
```

That is `testdata/dirty-retail` audited through MCP by a client running
`veritix mcp` as a subprocess.

## Client mode: the customer's own documents

The deterministic checks read the export and nothing else, and so does the
agent. That is a ceiling rather than a gap in the implementation: some defects
are not in the data.

`testdata/dirty-meters` is the fixture built to show it. Three meters are
`dormant`, `pending_install` and `decommissioned` in a column of ordinary
words; two were commissioned onto tariffs that had already closed; four reads
are lower than the previous read of the same meter; four meters are sited at
premises that do not exist. Nothing in the export marks any of these rows out.
The status column has six categories where three are permitted and nothing in
the data says which three. A falling register is a defect only if the column is
cumulative. `site_ref` is `UPN-4471` and `upn` is `4471`, so the names differ,
the shapes differ, and `relate.go` never compares them. Every one of these
becomes visible the moment somebody reads the data dictionary the customer
already maintains.

So Veritix connects to the MCP servers the operator configures, and offers its
model two tools: list what documents there are, and read one.

### Configuring it

```yaml
# veritix.yaml
context:
  max_document_bytes: 24000
  servers:
    - name: dictionary
      command: /usr/local/bin/dict-mcp
      args: ["--read-only"]
      env: ["DICT_TOKEN=…"]
    - name: tickets
      command: /usr/local/bin/tickets-mcp
```

There is no environment-variable form, deliberately: a list of subprocesses
with their arguments does not survive being flattened into one variable, and
this is the setting that says which programs Veritix will start. In a container
the file arrives on a mounted volume and `VERITIX_CONFIG` names it, which is
what `deploy/` already does.

Nothing is configured by default. A default install still talks to nobody.

For a run by hand, `audit` and `eval` take `--context-server name:command`
(repeatable) and `--no-context`, which ignores whatever is configured — that
second one is how to measure what the documents bought rather than assert it.

### Trying it

`scripts/context-server` serves a directory of Markdown as MCP resources. It is
not part of the product — a real deployment connects to the customer's own
dictionary or ticket system — but it is what the eval is measured with, and it
is the smallest complete example of what such a server has to do.

```sh
go build -o /tmp/ctx ./scripts/context-server
./bin/veritix audit testdata/dirty-meters --llm anthropic \
    --context-server "docs:/tmp/ctx -dir $PWD/testdata/dirty-meters/context"
```

### What leaves the process

A context server is the first thing since the model itself that anything leaves
this process toward, so the rule is the same shape as the one for SQL
identifiers, and it is structural rather than advisory:

**No text the model wrote reaches a context server.** Veritix enumerates each
server's resources at the start of the run and assigns each one a short id. The
model names a document by that id; the id is looked up and the *catalog's* URI
is what gets requested. An id that is not in the catalog is a tool error and
produces no request at all. The model is not shown the URIs either, because a
URI it can see is a URI it will invent.

The servers' own **tools** are not exposed, for the same reason: a tool call
forwards arguments the model wrote. That means a ticket system that can only be
searched cannot be reached from here yet. Turning it on is a decision about
egress rather than a feature to add, and it would need its own answer to "what
is in that search string" before it is worth having.

What does come back goes to the model **verbatim**. A data dictionary rendered
as `⟨XXXXXXX⟩` explains nothing, so the useful thing and the redacted thing
cannot be the same thing here — which is exactly unlike a cell value.
`redact.Guard.Document` is the one path that admits text untouched, and it
counts what it admitted. Configuring a context server is therefore the same
class of decision as `--include-values`: the operator takes it, and the run's
trace records every request Veritix made and every byte that came back.

That trace is the point. `/runs/{id}/trace` and `audit --trace-out` now carry a
`context` section listing each server, the catalog the model was offered, and
every request in order — each of them a listing, or a read of a URI that came
out of a listing. The web interface renders it on the trace screen, next to the
existing "what was the model sent" panel, because they are the same promise
read in two directions.

### What it is measured against

`dirty-meters`' manifest splits recall into **with context** and **unaided**.
The aided half answers whether fetching the documents worked. The unaided
half — two targets on the same runs of the same dataset that need no document —
is the control, because a transcript filling with documents is exactly how a
model stops doing the job it could already do. A run scoring worse on those
with the documents loaded has found a regression.

`ticket-4482.md` is a document no target needs, about a column that is fine. It
is there so the fixture measures reading the documents *against the data*
rather than reciting them.

## The dependency

The official `github.com/modelcontextprotocol/go-sdk` v1.7.0, measured before
adoption as `docs/frontend-stack.md` §6 asks. It adds **seven modules**: the
SDK itself, `google/jsonschema-go`, `segmentio/asm`, `segmentio/encoding`,
`yosida95/uritemplate`, `golang.org/x/oauth2`, and `golang.org/x/time`.
Licenses are Apache-2.0 (the SDK, transitioning from MIT) and MIT or
BSD-3-Clause for the rest — nothing copyleft, so nothing the commercial license
could not deliver.

Hand-writing it was considered and rejected. Unlike the OpenAI-compatible
provider — where there is no official SDK and the servers disagree about
corners a reference client would hide — MCP has a specification, a versioned
one, with a reference implementation in Go maintained alongside it. Owning
JSON-RPC framing, initialization, capability negotiation, and the schema
inference that turns a Go struct into a tool definition would be a standing
maintenance cost against a moving target, for no gain in auditability.

Client mode adds nothing: it is the same SDK, used from the other end.
