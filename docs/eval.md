# Scoring an audit against known defects

`veritix eval` audits a dataset whose defects are already known and reports
what was found and what was missed.

Without a model it measures the deterministic checks, takes a second, and is
what `make eval` and the test suite run. With a model it measures the model,
and that is the part worth explaining, because the obvious way to do it gives
an answer that is wrong in a way nobody notices.

```sh
make eval                                   # the checks, against the fixture

./bin/veritix eval testdata/dirty-retail \
    --llm openai-compatible \
    --llm-base-url http://127.0.0.1:11500/v1 \
    --llm-model gpt-oss-120b \
    --runs 5 --format json -o scores.json
```

## Why one run is not a measurement

An agent takes a different path every time. On `dirty-retail` there are two
defects nothing deterministic proposes — the region codes in `customers.csv`
and in the Q1 sheet of `sales.xlsx`, neither of which resolves against
`regions.csv`. Three runs of `gpt-oss-120b` are recorded in
[local-model.md](local-model.md): each found exactly one of the two, and never
the same one twice.

Read as a single number, each of those runs says "the model found a defect the
deterministic auditor cannot find", which is true and is the entire sales
pitch. Read together they say something quite different: the model checks one
relationship, finds it clean or dirty, and moves on. A clean
`check_referential_integrity` is evidence about *that pair*, not about that
column, and the model treats it as the latter.

So the scorecard reports two numbers and refuses to collapse them:

| | |
|---|---|
| **mean recall** | what one audit finds — the fraction of targets found, averaged over runs |
| **coverage** | what the runs find between them — the fraction found by at least one |

Half and half is a model that finds some defects and misses others. Half and
all is a model that finds a different one each time. They are different
products and they share a mean.

## Measuring what an accepted rule bought

The two numbers above are the argument for `propose_rule`, and until M6b
nothing could check that the argument paid off. `--rules` is that check:

```sh
./bin/veritix eval testdata/dirty-logistics --rules accepted.yaml
```

```
Deterministic checks
  9 of 9 planted defects found, 0 false positives
  3 defect(s) no check proposes; those are the agent's to find
  1 of those now caught by an accepted rule, with no model: shipments.delivered_before_dispatch
```

That line is the only figure on the scorecard that shows the return on paying
a model to audit data. A target listed there was the agent's to find on every
run, at the price of a model each time, and now is not — once per class of
defect rather than once per audit.

**It is reported apart from recall, and that separation is the point.** Mean
recall stays agent-origin only: it is a measurement of the model, and folding
in a rule somebody accepted last month would make a model look better every
time a human did some work. `MatchesTarget` is the same definition of "found
it" either way — the engine's number at the manifest's location — because a
rule that lands there has done the job whatever it is called.

The target also stays in the agent's list, marked. It is still scored against
the model, because the model's number has to keep meaning what it means; it is
no longer a hole in the product, and a list that did not say so would read as
one.

`TestAnAcceptedRuleConvertsAnAgentTarget` pins the whole of it against
`dirty-logistics`: no model at all, the rule a reviewer would have accepted for
`delivered_before_dispatch`, and the scorecard reporting exactly that one
target converted and the other three still open.

[rules-proposal.md](rules-proposal.md) is the loop this measures.

## The manifest

Ground truth lives beside the data, as `veritix-manifest.yaml` in the dataset
directory. It is one file rather than two: a second copy of a defect list
disagrees with the first eventually, and then a passing test means nothing.

```yaml
version: 1
dataset: dirty-retail

defects:
  - id: orders.negative_amount
    where: orders.csv.amount
    why: an amount cannot sensibly be negative
    caught_by: column.unexpected_negative

  - id: customers.region_orphans
    where: customers.csv.region
    why: >-
      four customer region codes do not appear in regions.csv. Joining
      customers to regions silently drops those customers from any
      regional total.
    caught_by: none
    agent:
      count: 4
      query: >-
        SELECT count(*) FROM customers_csv
        WHERE region NOT IN (SELECT region_code FROM regions_csv)

clean:
  - rule: column.type_violation
    where: orders.csv.order_id
    why: every order_id is a whole number
```

`where` is the location a finding reports: `<display>` or `<display>.<column>`.
`equivalent` is described under "counts that are stable" below.

`caught_by` names the deterministic rule that must catch the defect, or `none`
when nothing proposes it. A defect marked `none` and carrying an `agent` block
is a target: it is what the agentic tier exists for, and it is the only kind of
defect a model can score on.

`clean` is the other half, and it is not optional. A check that fires on
everything catches every planted defect, and only a list of places the data is
correct will notice.

### Defects that are not in the data

Some defects are invisible in the export and obvious the moment somebody reads
the customer's own documentation. `context` lists those documents and
`needs_context` says which targets depend on them:

```yaml
context:
  - id: data-dictionary
    file: context/data-dictionary.md
    why: the column definitions the customer maintains

defects:
  - id: meters.undocumented_status
    where: meters.csv.status
    why: >-
      three meters are in states the dictionary does not permit. Nothing in the
      data marks them out: they are ordinary words in a column of ordinary
      words, and none is a case variant of a permitted value.
    caught_by: none
    needs_context: [data-dictionary]
    agent:
      count: 3
      query: >-
        SELECT count(*) FROM meters_csv
        WHERE status NOT IN ('active', 'inactive', 'removed')
```

The documents live in a subdirectory of the dataset, in a form file discovery
does not recognize, so an audit ingests the CSVs and never sees them. What
reaches a model is whatever fetches them — which, once M5b lands, is Veritix's
MCP client pulling from the customer's own systems.

Three rules hold them together:

- **A document states the rule and never the violation.** One naming the
  offending row would be handing over the answer, and the fixture would be
  measuring whether a model can copy an identifier out of a paragraph.
- **`needs_context` on a defect `caught_by` a check is refused.** A check reads
  the export and nothing else, so a defect cannot be both.
- **A document has to mention the column its target names.**
  `TestAnAidedTargetsDocumentsMentionItsColumn` pins it. The failure it catches
  is the fixture drifting: somebody rewrites the dictionary, the sentence that
  made the defect visible goes, and from then on every run scores zero on that
  target — which looks exactly like a model that did not look.

### Two things the manifest does on purpose

**Credit needs the location *and* the engine's number.** A model's rule slug
and its title are prose; two runs describe the same defect two different ways,
and scoring against prose would measure vocabulary. What the engine measured
from the model's own `count_query` is not prose. A finding at the right place
measuring the right number is the same claim however it was worded — and a
finding measuring something else about the same column is not that claim, which
is what the count is there to catch.

**The manifest's own counts are re-run.** `agent.query` is the SELECT that
returns `count`, and `TestAgentTargetCountsAreMeasurable` runs it against the
fixture. It is never shown to the model and it is not how a finding is
credited: the model has to write its own query and the engine has to run that
one. It is there because a target with a wrong count is a target nothing can
ever match, and the eval would report every model missing it forever with
nothing saying why. Evidence that re-runs is the rule everywhere else in
Veritix and a manifest making claims about a dataset is a poor place to start
excepting things from it.

## What the scorecard says

This is a real run: `qwen3:4b-instruct-2507-q4_K_M` under Ollama, on a
four-core i5-7300U with no GPU.

```
Dataset: dirty-retail

Deterministic checks
  22 of 22 planted defects found, 0 false positives
  2 defect(s) no check proposes; those are the agent's to find

Agent  qwen3:4b-instruct-2507-q4_K_M via openai-compatible, 3 runs, 14 steps max
  mean recall     17%   what one audit finds
  coverage        50%   what 3 runs find between them
   0/3   customers.region_orphans     customers.csv.region
         four customer region codes do not appear in regions.csv: APAC,
         the placeholders 'N/A' and '-', and 'emea' lowercased. Joining
         customers to regions silently drops those customers from any
         regional total.
   1/3   sales.region_orphans         sales.xlsx#Q1.region
         APAC and LATAM appear in the Q1 sheet's region column and in
         neither case in regions.csv. ...

Runs
   1  0 of 2 found, 14 steps, 14 tool calls, 0 recorded, 139511 tokens, 24m43s (step_budget)
   2  1 of 2 found, 14 steps, 13 tool calls, 1 recorded, 122100 tokens, 28m2s
   3  0 of 2 found, 14 steps, 14 tool calls, 0 recorded, 139531 tokens, 24m38s (step_budget)
```

**17% and 50% are the point.** A single run of this model reports either 0% or
50% depending on which run you happened to take, and both readings would have
been quoted as "the model finds the defect the deterministic auditor cannot" or
"the model finds nothing". Neither is the answer. The answer is that it finds
one of the two about a third of the time and the other one never.

A scorecard can also carry two lists that are reported and not scored. This run
produced neither; they are worth reading rather than scoring when they appear.

**Already found by a check** is not a mistake and is not the job either. The
check tools tell the model when a defect is already covered, and this line is
how well that lands. A model spending its budget restating the deterministic
report is doing nothing wrong and nothing useful.

**Not on the manifest** is true. Every agent finding was measured by the engine
and re-verified by `finding.Set.Verify` before it reached the report, so these
are real statements about the data; nobody planted them. Whether that makes one
a defect the manifest should gain or a model calling something trivial a
problem is a judgment, so the scorecard shows them and declines to grade them.

**Known not to be a defect** is that judgment, once somebody has made it,
written into the manifest so it does not have to be made again on every run:

```yaml
noise:
  - where: shipments.csv
    count: 4
    why: >-
      status mixes in_transit with delivered and returned, so four rows differ
      in length and shape from the rest. That is the enum, not a formatting
      defect.
```

A `noise` entry is keyed the way a target is keyed — the engine's number at a
location — and for the same reason. It cannot be keyed on the rule name,
because that is the half of a claim the model writes: `gpt-oss-120b` reported
this one as `inconsistent_status_length` on one run and `mixed_status_format`
on another, and a `clean` entry naming either would have matched one run and
missed the other. `clean` polices the *checks*, whose rule names Veritix chose,
and cannot be stretched to cover an agent claim.

It labels and does not penalize. Marking a model down for noticing something
true would be grading its judgment through its wording, which is what credit by
location-and-count exists to avoid. It also cannot absolve a real hit: a claim
only reaches this list after failing to match every target, and `Validate`
refuses a noise entry that measures a target's count at a target's location —
the same collision `equivalent:` produced the first time it was reached for.

## The gate

A missed planted defect or a check firing on clean data exits non-zero without
being asked to. The manifest is not an opinion, and both are unambiguous
regressions.

The model's score is not treated that way. `--min-recall 0.5` opts in to a
threshold; without it a low score is reported and the command succeeds. A build
that fails because a model had a bad afternoon is a build people learn to
ignore.

## What an eval run is, exactly

It is an ordinary audit. `eval.Run` drives `audit.Run` — the same pipeline the
CLI, the HTTP API and the MCP server drive — so what is being scored is the
auditor a customer runs and not an arrangement that exists only in the eval.
Each run's trace is carried into the JSON scorecard, so a score kept for months
still answers what the model was actually sent.

One thing is forced rather than configured: **an eval never lifts the egress
policy**, whatever `llm.allow_sample_values` says. A score obtained by showing
the model cell values is not a score for the product anybody ships.

## The three fixtures, and why there are three

They measure different things on purpose.

**`testdata/dirty-retail`** has two agent targets and both are unresolved
references. `check_referential_integrity` finds either one in a single call, so
what this fixture measures is whether a model will *use a tool surface it was
not asked to use*. That is a real and discriminating question:
`qwen3:4b-instruct-2507` uses the check tools and scores; `qwen3.5:35b-a3b` is
eight times the size and answered three whole runs with nothing but `run_sql`.

**`testdata/dirty-logistics`** has four, and no check tool measures any of
them:

| target | kind |
|---|---|
| `shipments.delivered_before_dispatch` | two columns of one row contradict each other |
| `shipments.weight_in_grams` | three values are in the wrong unit, none implausible alone |
| `invoices.currency_contradicts_column` | a column's name contradicts the column beside it |
| `invoices.issued_before_dispatch` | the contradiction only exists across a join |

Each needs the model to read the profile, form a hypothesis about what the
columns are supposed to mean to each other, and write the SQL that would settle
it. A model can score full marks on `dirty-retail` with four tool calls and
score zero here.

Measured, three runs of `gpt-oss-120b`: **mean recall 42%, coverage 75%**, with
the four targets landing at four different rates — `weight_in_grams` 3/3,
`delivered_before_dispatch` and `currency_contradicts_column` 1/3 each, and
`issued_before_dispatch`, the one that only exists across a join, 0/3. That
spread is what more than one target in reach at once buys: `dirty-retail` has
one reachable target per column, so it can report a fraction but never say which
kind of reasoning a model has and which it lacks. No run was stopped by a
budget — all three finished voluntarily at 10 to 12 steps of 24 — so on this
model the ceiling is the stop decision, not the step count.
`docs/local-model.md` has the call sequences.

Its deterministic half is worth having too. `relate.go` proposes
`shipments.dest_site → sites.site_code` on its own, from a naming convention
that shares no word with the parent key — which is evidence the relationship
inference generalizes past the fixture it was written against.

**`testdata/dirty-meters`** is the first fixture whose defects are not all in
the data. Four of its six agent targets are invisible in the export and become
visible only when the customer's own context is read:

| target | what makes it visible | kind of context |
|---|---|---|
| `meters.undocumented_status` | three meters are in states the dictionary does not permit | a vocabulary |
| `meters.retired_tariff` | two meters were commissioned onto tariffs already closed to new meters | a lifecycle date, and there are two of them |
| `readings.register_went_backwards` | `register_value` is a cumulative register, so it cannot fall | what a column means |
| `meters.site_ref_orphans` | `site_ref` is the premises `upn` with a `UPN-` prefix | how two columns join |

None of the four has an internal signal. `dormant` is as plausible a status as
`active`; `STD-A` is the most common tariff in the file and still a valid one,
because the meters already on it are still billed on it; a falling register is
only wrong once you know the column is an odometer rather than a trip meter;
and nothing suggests `site_ref` and `upn` are the same thing written two ways —
the names differ, the shapes differ, and `relate.go` never compares them.

The other two targets are ordinary, and that is the point. A fixture where the
context is the only way to score anything can show that fetching it helped and
cannot show what it cost — and a transcript filling up with documents is
exactly how a model stops doing the work it was already doing.
`readings.read_before_install` and `readings.register_wider_than_meter` need no
document, so they are the control, measured on the same runs of the same
dataset. The scorecard prints the two recalls under the overall pair — this is
the shape of the output, not a measurement:

```
  mean recall     ..%   what one audit finds
  coverage        ..%   what 3 runs find between them
    with context  ..%   over 4 targets needing a document
    unaided       ..%   over 2 targets the export alone can answer
```

### Running it

The documents live in `context/`, which `source.Discover` does not recognize,
so an audit ingests four CSVs and never sees them. What reaches a model is
whatever Veritix's MCP client fetches, and that needs a server to fetch from.
`scripts/context-server` is one:

```sh
go build -o /tmp/ctx ./scripts/context-server
./bin/veritix eval testdata/dirty-meters --llm anthropic --runs 3 \
    --context-server "docs:/tmp/ctx -dir $PWD/testdata/dirty-meters/context"

# the control, on the same command line
./bin/veritix eval testdata/dirty-meters --llm anthropic --runs 3 --no-context
```

`--no-context` ignores whatever is configured, which is what makes the second
run a control rather than a different experiment. `docs/mcp.md` has the client
half in full: what leaves the process, and what does not.

**No model has been run against this fixture yet.** The deterministic half is
measured: 8 of 8 planted defects, no false positives. The aided half scored
zero before there was an MCP client, which is the baseline worth having — a
number to improve on rather than a claim.

A run that scores worse unaided with the documents loaded than without them has
found a regression, not a feature, and without the second number that would
have shown up as the aided half looking good.
`TestAFixtureWithContextAlsoCarriesAControl` is why every future fixture with
context has to carry a control too.

A third document, `context/ticket-4482.md`, is a closed ticket about a column
that is fine. No target needs it. It is there so the fixture measures reading
the documents *against the data* rather than reciting them.

## Adding a dataset

A third dataset is worth more than a third model. What one needs to be worth
scoring:

- **More than one target in reach at once**, so partial credit means something.
  `dirty-retail` has one reachable target per column, which is why three
  `gpt-oss-120b` runs could not distinguish "found the defects" from "found *a*
  defect and stopped".
- **Targets of different kinds**, so a model that only knows how to check
  referential integrity scores differently from one that reads a profile.
- **A `clean` list that is honestly hard** — places that look wrong and are
  not, and especially the neighbor of something that is wrong. A check that
  generalized from `dest_site` to `origin_site` would catch every planted
  defect in `dirty-logistics` and be useless.
- **An unambiguous location.** Credit needs the finding to land at the target's
  `where`. A defect that spans a join has two plausible homes, so put it where
  the wrong value actually is — the invoice is what is dated too early, not the
  shipment.
- **Counts that do not depend on phrasing.** This is the one that bites.
  Veritix's own `reference.orphan_values` counts *distinct* offending values; a
  model writing `count(*)` counts *rows*. Where those disagree, one of two
  correct models is refused credit and it looks like the model's failure.
  `TestAgentTargetCountsDoNotDependOnPhrasing` checks it, and every target in
  `dirty-logistics` has row count equal to distinct count by construction.
  Building `dirty-meters` it caught the first draft's retired-tariff target:
  three meters on one closed code count 3 rows and 1 distinct value. The
  fixture was the thing that changed — a second tariff was retired, so the
  target became two meters on two codes.
- **No two targets in one table sharing a count.** `MatchesTarget` lets a
  finding scoped to a whole table cover any column in it, deliberately, because
  scoring a model's prose strictly would measure phrasing. That concession has
  a price: three targets in one table all measuring 2 means a table-scoped
  finding about any of them is credited to whichever the manifest listed first,
  and the per-target rates — the whole reason for repeating runs — start
  attributing hits to the wrong defect. `dirty-meters` keeps the counts within
  each table distinct.

  Where a target genuinely admits two true figures, `equivalent: [n]` lists the
  others. Use it sparingly: it widens what counts as a match, and the first
  time it was reached for it produced a collision with a different defect at
  the same location — two orphaned rows sharing one code measure 1, and so did
  a `column.missing_values` finding on the same column. The fixture was changed
  instead.
