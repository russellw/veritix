# Veritix over MCP

`veritix mcp` serves Veritix's audits to an assistant — Claude Code, Claude
Desktop, anything speaking the Model Context Protocol — over stdin and stdout.
Ask the assistant to audit a folder, and it calls the same pipeline the web
interface drives.

This is the M5a half of the milestone: Veritix as an MCP **server**. Client
mode, where Veritix's own agent pulls the customer's context (data
dictionaries, ticketing, warehouse metadata) into an audit, is M5b.

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
