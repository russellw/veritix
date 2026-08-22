# The rule the model proposes, and the person who accepts it

An agentic audit costs money and gives a different answer every time. This is
the part of Veritix that makes it pay for itself: the model's second output is
not a finding but a *rule*, and once a person accepts one, the deterministic
pass enforces it on every later audit of that dataset — with no model, no
tokens and no variance.

```sh
# propose, from the command line
./bin/veritix audit testdata/dirty-retail --llm anthropic \
    --propose-rules-out proposed.yaml

# review it in an editor, strike out what is wrong, then
./bin/veritix audit testdata/dirty-retail --rules proposed.yaml
```

In the browser it is the same three steps with the review done on a screen
rather than in an editor, and the accepted rule stored against the dataset so
that every later audit picks it up without anyone remembering a flag.

## Why a second output at all

`veritix eval` reports two numbers and refuses to collapse them
([eval.md](eval.md)). On `dirty-logistics`, `gpt-oss-120b` scored **42% mean
recall against 75% coverage** over three runs: what one audit finds, against
what three audits find between them. The whole of that gap is defects the model
found on one run and missed on the next two.

Read as a product that is a bad position. Paying per audit for a 42% chance of
being told about a defect is not something to sell to a business, and the
obvious fixes — run it three times, use a bigger model — buy a few points at
several times the price.

The gap closes from the other end. A defect found *once* is enough, if what
comes out of that run is an expectation rather than an observation:

| | |
|---|---|
| `record_finding` | this data is wrong now — rows, a count, evidence that re-runs |
| `propose_rule` | this expectation should hold every time — a rule, for a person to accept |

A finding is consumed by the run that produced it. A rule outlives it. That
converts coverage into recall permanently, once per class of defect rather than
once per audit, and it does it in the direction that gets cheaper over time.

## What a proposal is

A `rules.Proposal` is an ordinary `rules.Rule` — the same document type a
customer writes by hand — plus the argument for it and what it measured when it
was proposed. It is not applied, and nothing in Veritix applies one.

The tool takes the same discipline `record_finding` takes, because the
temptation is identical: a model that can write an expectation can write one
that sounds right and measures nothing. So Veritix compiles the proposal into a
real `rules.File`, materializes whatever the model could not see, and runs it
through the real `rules.Evaluate` against the data in front of it. The model
states `violations_now`; if the engine disagrees, **nothing is proposed** and
the model is handed the real figure.

One thing is deliberately *not* carried over from findings — the zero rule:

- A **finding** whose count query returns zero is refused. A problem that does
  not reproduce is not a problem.
- A **proposal** that nothing violates today is the best kind of rule there is.
  "status is drawn from these four values" is worth accepting precisely because
  it holds now and should keep holding.

What has to be refused instead is the true analogue of a finding that does not
reproduce: a rule that matches no column. That one would sit in a customer's
rules file forever looking like protection, and `rules.Evaluate` would report
it as `rule.never_applied` on every run — which is the same information, a
year late.

## `one_of`, and the values the model is never shown

The most valuable rule kind is the one a model cannot write. `expect: one_of`
restricts a column to a fixed vocabulary, and its body is literally a list of
cell values — which the egress guard never shows a model. Under the default
policy even `sample_values` comes back as shapes.

The answer is not to relax the guard for rule proposal. It is to split the
rule in two:

1. The **model proposes the shape** of the expectation: *status is drawn from a
   fixed vocabulary*. It supplies no values and is shown none.
2. **`rules.Materialize` fills in the contents**, in the customer's process,
   from the customer's own column, bounded to
   `rules.MaxMaterializedValues` (50) so that what a person is asked to accept
   is something they can read. Four thousand distinct values is a copy of the
   column, not a vocabulary.
3. The **person reviews the concrete list**, and that review is the point.

The tool result tells the model how many values were filled in and never what
they are; `TestAProposalsValuesNeverReachTheModel` pins it.

Step 3 is not a formality. A set materialized from a column contains whatever
the column contains, mistakes included. On `dirty-retail` the proposed
vocabulary for `status` reads:

```
active   Active   ACTIVE   Actve   Inactive
```

`Actve` is one of the defects. Accepting that rule unread would enforce the
misspelling forever — the rule would pass on every future audit *because* the
typo is permitted. Unchecking it is what turns the same rule into the thing
that catches it. This is why the accept step shows values, why it shows them
as checkboxes, and why no part of the path applies a proposal automatically.

## What identifies a proposal

A proposal's id is a digest of **what the rule asserts** — table, column,
expectation, pattern, bounds, `WHERE` clause — and not of how it was worded.

The slug, the description and the rationale are all prose the model writes, and
two runs word the same expectation two ways. The materialized values are left
out for a different reason: they are read from data that changes between runs,
and `one_of` on `status` proposed in March and again in June is one proposal to
review, not two, even where the column has since acquired a fifth spelling.

The consequence a user sees: accepting a rule and reloading the run marks that
proposal as in force rather than offering it again and refusing it on press.

## Three ways to review one

### The report

Every audit that proposed rules carries them in `report.Document`, in their own
section: the text report prints them, the JSON report carries them as
`rule_proposals`, and the HTML report lists them under the findings. What a
report carries is the **shape** of each rule and a **count** of what it
permits, never the values — a report is a file that gets emailed and pasted
into tickets. SARIF has no section for them and should not grow one: it is a
format for defects in a scan, and a proposal is not one.

### The command line

`--propose-rules-out FILE` writes the proposals as a rules file: the same
document `--rules` loads, ready to read and edit.

This is the one place a proposal's permitted values are written out, and
deliberately so — a `one_of` rule without its values is not a rule at all. It is
written when somebody asks for it, to a path they named, on their own machine,
like every other path to a verbatim value. The file carries a header saying
that nothing in it is in force, because a file of rules that looks
authoritative and is not is worse than no file.

The flag is refused without a model configured, before the audit rather than
after it. A run that proposed nothing still writes the file, saying so.

### The browser, which is where accepting happens

`GET /runs/{id}/proposals` lists a run's proposals **described** — through the
same `report.DescribeProposal` the report uses, so the screen and the
downloaded report cannot disagree.

`GET /runs/{id}/proposals/{pid}` returns one in full, including the values it
would permit. **This is one of exactly two endpoints in Veritix that serve raw
customer data**, the other being the per-finding rows endpoint, and it is
bounded the same way: one named thing at a time, asked for by id, never inside
a list response, never logged. The interface fetches it only when a reviewer
presses on one proposal.

`POST /datasets/{id}/rules` accepts it. The request body *is* the review: the
name, the description, the severity and the permitted values as the reviewer
left them. Severity is always sent rather than inherited, because a rule that
can fail a build should do so because somebody chose that.

## What accepting does

The accepted rule is written to `<DataDir>/datasets/<id>/rules.yaml`, and
`runs.AcceptedRules` loads it on **every** later audit of that dataset — over
HTTP and over MCP alike, whether or not a model is configured, whether or not
anyone remembers it exists.

`--rules` stays the customer's own file and the two are additive.
`runs.Merge` refuses a name collision rather than letting one file silently
redefine the other's rule, which is why `POST` answers `409` on a name already
in force and why the accept screen lets a reviewer rename.

The dataset screen lists what is in force, named the way the rules file names
it — the SQL table a rule is written against, not the source name the proposal
screen showed. That list is a view of a file a person can edit.

## The measurement

`TestAnAcceptedRuleIsEnforcedWithoutTheModel` is the whole loop in one test: a
model proposes a vocabulary rule on run one, a person accepts it with the
misspelled status struck out, and run two — with no model configured at all —
reports `rule.status_domain` against the one bad row.
`e2e/tests/proposals.spec.ts` is the same three steps in a browser, ending with
a second audit run with the model unchecked.

Both use a scripted model, because what they test is what Veritix does with
what a model said. What a real model actually proposes is measured by hand, and
those measurements are in [local-model.md](local-model.md).

## What this is not

Nothing here applies a rule. An accepted rule raises errors on future data and
can fail a CI gate, and that is not something a model gets to do unattended.
The model's authority stops at the suggestion; every step after it is a
person's, and the design spends its effort on making that person's decision an
informed one rather than on making it unnecessary.
