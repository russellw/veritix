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

```
Dataset: dirty-retail

Deterministic checks
  21 of 21 planted defects found, 0 false positives
  2 defect(s) no check proposes; those are the agent's to find

Agent  gpt-oss-120b via openai-compatible, 5 runs
  mean recall     50%   what one audit finds
  coverage       100%   what 5 runs find between them
   3/5   customers.region_orphans     customers.csv.region
   2/5   sales.region_orphans         sales.xlsx#Q1.region

  Also recorded, where a check had already found it:
    agent.negative_order_amount      orders.csv.amount     column.unexpected_negative

  Recorded and not on the manifest, measured by the engine:
    agent.lowercase_currency         orders.csv.currency   1 row(s)

Runs
   1  1 of 2 found, 18 steps, 24 tool calls, 2 recorded, 1 refused, 257k tokens, 59m
```

The two lists at the bottom are the ones worth reading rather than scoring.

**Already found by a check** is not a mistake and is not the job either. The
check tools tell the model when a defect is already covered, and this line is
how well that lands. A model spending its budget restating the deterministic
report is doing nothing wrong and nothing useful.

**Not on the manifest** is true. Every agent finding was measured by the engine
and re-verified by `finding.Set.Verify` before it reached the report, so these
are real statements about the data; nobody planted them. Whether that makes one
a defect the manifest should gain or a model calling something trivial a
problem is a judgment, so the scorecard shows them and declines to grade them.

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

## The two fixtures, and why there are two

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

Its deterministic half is worth having too. `relate.go` proposes
`shipments.dest_site → sites.site_code` on its own, from a naming convention
that shares no word with the parent key — which is evidence the relationship
inference generalizes past the fixture it was written against.

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

  Where a target genuinely admits two true figures, `equivalent: [n]` lists the
  others. Use it sparingly: it widens what counts as a match, and the first
  time it was reached for it produced a collision with a different defect at
  the same location — two orphaned rows sharing one code measure 1, and so did
  a `column.missing_values` finding on the same column. The fixture was changed
  instead.
