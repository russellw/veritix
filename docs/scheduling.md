# Auditing on a schedule

The comparison answers "what changed since the last audit". Something has to
run the last audit.

Until a schedule exists, that something is a person: open the browser, pick the
dataset, press Run. The people Veritix is for — business users on Windows
desktops, which is why there is a web interface at all — are not going to do
that every morning, so the comparison only ever fires when somebody already
suspected a problem. That is the one case where they did not need telling.

A schedule turns Veritix from a tool somebody runs into a thing that runs. The
export lands overnight, the audit happens at two, and if the export got worse
somebody hears about it before the working day starts.

```yaml
# In the server's configuration: the clock, and where to tell somebody.
schedule:
  enabled: true
  tick: 30s

notify:
  webhook_url: https://example.com/hooks/veritix   # empty is off
  on: regression                                   # regression | failure | any
  min_severity: error
  detail: findings                                 # findings | summary
  base_url: https://veritix.example.com            # so a message can link
  timeout: 10s

server:
  retain_databases: 336h   # 14 days, in hours: a Go duration has no day unit
```

Then, per dataset, on the dataset screen: **Audit on a schedule** — every day,
every week, or every few hours, at a time of day in a time zone you choose,
with an optional "tell me when it gets worse".

Over HTTP it is three calls:

```sh
DS=<dataset id>
curl -s -XPUT localhost:8080/api/v1/datasets/$DS/schedule \
     -H 'Content-Type: application/json' \
     -d '{"kind":"daily","at":"02:00","timezone":"Europe/London","notify":true}'
curl -s localhost:8080/api/v1/datasets/$DS/schedule | jq .next_due_at
curl -s -XDELETE localhost:8080/api/v1/datasets/$DS/schedule
```

## Why a schedule and not a watcher

The obvious alternative is to watch the folder and audit when it changes. It is
the wrong thing to build first, because **a watcher fires while the export is
still being written.** Deciding that a 900 MB CSV has stopped being copied means
guessing — no writes for thirty seconds, unless the share stalled — and a wrong
guess produces a confident audit of a truncated file, which is exactly the
failure `column.not_profiled` exists to refuse.

"Daily at 02:00" is the customer asserting that the export lands by 01:00. That
is knowledge Veritix cannot derive and they already have.

## What a schedule is

Every day at a time, every week on a weekday at a time, or every so many hours.

**There is deliberately no cron syntax.** A cron string is a parser this
project would have to justify as a dependency, and it is unwritable by the
people the interface exists for. If an operator ever needs one it is a fourth
kind on the same structure, not a different mechanism.

**The time of day is read in the schedule's own time zone**, not the server's.
"Overnight" is a fact about the business whose export this is, not about the
machine Veritix happens to run on, so the zone travels with the schedule. The
web interface fills in the browser's own zone, which is right in the ordinary
case and editable when it is not.

Twice a year that has consequences, and Veritix takes the boring option both
times:

- **The night the clocks go forward**, an hour does not exist. A schedule set
  for 01:30 in a zone that skips 01:00–01:59 runs at the next moment that does
  exist — 02:30 — rather than silently missing that night.
- **The night they go back**, an hour happens twice. The audit runs **once**.

An *interval* schedule is the other choice on purpose: it counts elapsed time,
so "every 24 hours" drifts by an hour across a transition where "daily at 03:00"
does not. Use a daily schedule when you mean a time of day.

## What happens when a window comes round

The clock looks for due schedules every `schedule.tick`, thirty seconds by
default. A window that has arrived does one of four things, and each is
recorded on the schedule where the dataset screen shows it:

- **It starts an audit.** An ordinary run: in the same history, on the same
  event stream, cancelable from the same screen, with the same accepted rules
  in force and the same comparison against the previous run.
- **It skips, because an audit of that dataset is already running.** Two
  gigabytes takes about fifteen minutes; an hourly schedule on something slower
  must not stack.
- **It skips, because it was missed by more than its own period.** A server
  that was off over its 02:00 audit and came back at 09:00 runs one now. A
  server that has been off for a week has not missed one window but seven, and
  auditing week-old state the moment it starts is not what "daily at 02:00"
  asked for; the schedule resumes at its next window instead.
- **It fails to start**, because the dataset's path has gone — an unmounted
  share, a renamed folder. The reason is recorded and shown, because a schedule
  that has been failing quietly for a month is worse than no schedule at all.

Whatever happens, the window advances to the next one in the future. A missed
window fires **once**, not once for every tick that finds it.

**At most one audit starts per tick.** That is the whole of the stagger: a
server coming back after a weekend with fifteen datasets due starts one every
thirty seconds rather than fifteen at once, and nothing is starved, because a
schedule that fires moves its window on and the one left behind is still due.

### What a scheduled audit does not do

**It runs no model**, even where a provider is configured. Sending a dataset's
metadata to a model unattended, every night, forever is exactly the decision
the per-run switch exists to make deliberately. A scheduled audit is the
deterministic auditor: the checks, and whatever rules are in force.

**It includes no cell values.** `--include-values` is a decision somebody takes
when they are about to send a report somewhere, and nobody is here.

### Only a dataset registered by path

An uploaded dataset is a copy of the data as it was at the moment somebody sent
it, and it does not change again. Auditing it nightly would produce the same
report forever and a comparison that never said anything — which looks exactly
like a schedule that is working. The API refuses one, and the interface does
not offer the panel.

## Being told

A webhook, and only when it is configured: `notify.webhook_url` empty is the
default, and a default install tells nobody, the same way it talks to no model
and exports no telemetry. Setting a URL is the operator's switch; the checkbox
on each schedule is which datasets use it.

Webhook and not email, deliberately. Teams, Slack, PagerDuty and a two-line
script all accept one, where email means credentials, TLS modes, sender
identity, recipient lists, retries and bounces — reimplementing a bridge every
customer already has.

`notify.on` decides what makes a message:

| | |
|---|---|
| `regression` | New or worsened findings at or above `min_severity` — **and** a run that could not complete. The default. |
| `failure` | Only a run that could not complete. |
| `any` | Every scheduled run, including a clean one. |

A failed run counts as a regression whatever the setting, and that is not
overloading the word: a run that produced no report cannot be shown *not* to
have regressed, which is the argument `rule.never_applied` makes about a check
that never ran. A nightly audit that has been failing for a month is precisely
the thing nobody notices.

There is no "every night, whether or not anything happened" default for the
same reason: a nightly message saying nothing changed is how a channel gets
muted, and then the one that mattered is muted too.

### What is in a message

```json
{
  "event": "regression",
  "dataset": "monthly-extract",
  "run_id": "01a0…",
  "status": "succeeded",
  "url": "https://veritix.example.com/runs/01a0…",
  "findings": { "total": 21, "errors": 9, "warnings": 8, "info": 4 },
  "changes": { "new": 0, "worsened": 1, "resolved": 0, "unchanged": 20 },
  "regressions": [
    {
      "rule": "reference.orphan_values",
      "severity": "error",
      "status": "worsened",
      "title": "2 value(s) in orders.csv.customer_id have no matching row in customers.csv.customer_id",
      "table": "orders_csv",
      "column": "customer_id",
      "affected_count_before": 1,
      "affected_count_after": 2
    }
  ]
}
```

The report's own comparison section, and nothing else. **Never a cell value,
never an offending row, never the SQL behind a finding.**

That titles and locations are admissible here, where a telemetry span refuses
even a table name, is a decision about audiences: a span is an access log
leaving the machine for a collector, and this goes to the people whose data it
is. "3 new errors" with no location is not something anybody can act on, which
makes it one more message nobody reads. `detail: summary` drops the titles and
locations, for an operator posting into a channel wider than the data's own
audience; the dataset's name stays, because a message that does not say which
dataset is not actionable at all.

The promise that no cell value can reach a webhook does not rest on care taken
while writing the message. It rests on two decisions made elsewhere: only a
scheduled run notifies, and a scheduled run passes neither `--include-values`
nor a model. So the document a message is built from is a deterministic report
with values off — the same document the four report writers are held to.
`TestNoNotificationCarriesCustomerData` reads the bytes that actually left the
process, against the same list of fixture values the report and agent egress
tests use.

### When the webhook is down

Three attempts, a second and then five seconds apart, and then the message is
lost and a line goes in the log. **A delivery failure is never a run failure.**
The audit is done and its findings are recorded; an audit that died because a
chat server was down would be worse in every way. It is the same rule a context
server that will not answer gets.

A webhook outside the cluster needs a NetworkPolicy rule of its own: the
shipped manifests deny egress by default.

## Keeping the disk

Every run leaves a DuckDB copy of the ingested dataset behind, so that a
finding's offending rows can be fetched afterwards. It is roughly a third of
the dataset's size. Auditing by hand never made that a problem; **a nightly
audit of a two gigabyte export is about 700 MB a night**, which fills a disk in
a quarter.

`server.retain_databases` — 14 days by default, `0` to keep everything — is
what stops that. What it discards is the ingested copy of the customer's own
files, which can be made again by auditing them again. What it keeps is the
run, its report, its findings and its trace: that is the audit trail, it is
small, and it is what somebody wants six months later.

Two consequences worth knowing:

- **The comparison is unaffected.** It reads the stored report document and
  never the DuckDB file, which is what makes this split possible at all. A run
  whose data was discarded is still a baseline.
- **Asking for that run's offending rows answers `410 Gone`**, with a sentence
  saying so, rather than an error that reads as a broken server. The most
  recent run of each dataset keeps its data whatever the cutoff says, so the
  newest audit's rows are always there to look at.

## One instance, one clock

`replicas: 1` was already the shape of a Veritix deployment: a run's DuckDB
file and the SQLite audit trail are on the pod's volume, so a second replica
would serve a different history and answer a rows request from the pod that
does not have the file.

A schedule adds a second reason. Two replicas sharing a data directory would
both find the same window due and both start an audit. If a process must not
run the clock — a second Veritix beside the one that does, or `veritix mcp`
sharing a data directory — `schedule.enabled: false` stops it firing anything
without touching a single stored schedule. `veritix mcp` never ran one anyway:
it does not build a server.

Expired run databases are discarded whichever way that switch is set. An
operator who turned the clock off because another process owns it has not asked
for the disk to fill up.
