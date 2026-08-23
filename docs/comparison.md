# What changed since the last audit

A report says fourteen errors. That is a number. What somebody needs to know on
the second audit and every one after it is whether it was twelve last week, and
whether the three they fixed on Tuesday stayed fixed — and no single run can
answer either question.

Every audit of a dataset that has been audited before carries a **comparison**:
what is new, what got worse, what was resolved, what improved, and what drifted
in the shape of the data itself. Nothing has to ask for it in the browser or
over MCP. From the command line it takes a baseline, because `veritix audit`
keeps no history of its own.

```sh
# In CI: keep the report, compare against the last one, and fail only on
# what this change introduced.
./bin/veritix audit ./data --format json -o report.json
./bin/veritix audit ./data --baseline last-report.json \
    --fail-on-regression error
```

In the browser it is a strip under the finding counts and a **Changes** tab
beside them. Over HTTP and MCP it is the `comparison` field of the report
document, present whenever there is an earlier successful run of the same
dataset to compare with.

## What it compares

**Two report documents.** Not two sets of rows in a database — the document is
what every entry point already produces, and what the server already stores
whole. A diff computed from it cannot disagree with the report it sits in,
which is the same reason the HTTP API serves the stored document verbatim
instead of rebuilding it. It is also why the CLI can do this at all: a JSON
report from a previous run is a file, and a CI job already keeps one as an
artifact.

**Findings are matched by id.** A finding's id has been a digest of *what the
finding is about* — its rule, table, column and line — rather than where it
landed in a list, since the run store was built. That is what makes "the same
problem" a question with an answer: an id survives a re-run that turns up one
more error beside it.

Each matched pair is classified by its affected count:

| | |
|---|---|
| `new` | this run has it and the baseline did not |
| `worsened` | the same problem, affecting more rows than before |
| `resolved` | the baseline had it and this run does not |
| `improved` | the same problem, affecting fewer rows |
| `unchanged` | same problem, same count |

A severity that moved is reported separately, because a count cannot show it: a
rule whose severity somebody edited between the two runs is the same finding
about the same rows, now able to fail a build.

Unchanged findings are counted and not listed. They are already in the
report's own findings list, and repeating every one of them would double the
document in order to say that nothing happened.

## The half that is not about findings

The comparison also reports **volume and schema drift**: a table that appeared
or vanished, a row count that moved, a column that is no longer in the export.

None of this is a finding and none of it can be. Every check in Veritix reads
one audit, and an export that quietly lost a third of its rows looks completely
healthy to all of them — the remaining rows are as clean as they ever were. It
is usually a broken extract rather than a business event, and it is often a
worse problem than anything in the findings.

It is also the honest limit of the finding comparison, and the two are reported
together for that reason. A finding is identified partly by where it sits, so a
column that leaves the export takes its findings with it and they appear as
resolved. When that happens the comparison says so in a note, above the list it
is about: *check the table changes before reading that as data that was cleaned
up.*

## The CI gate

`--fail-on` fails a build on the state of the data. `--fail-on-regression`
fails it on the *direction*:

```sh
./bin/veritix audit ./data --baseline main-report.json --fail-on-regression error
```

This is what makes the comparison worth having in a pipeline. A team that has
just pointed Veritix at fifteen years of accumulated data cannot fix everything
today, and a gate that fails on all of it is a gate somebody switches off in a
week. They can still refuse to make it worse.

It counts findings that are **new or worsened** at or above the severity given.
Worsened counts deliberately: three orphan references becoming three hundred is
not the same problem at a different size, it is a problem that got away from
somebody.

Without `--baseline` it is refused rather than passing silently. A gate with
nothing to compare against would go green on every build, which is the worst
thing a gate can do — it reads as clean rather than as never having run. That
is the same argument `rule.never_applied` makes about a rule that matched
nothing.

## Which run is "the previous audit"

The most recent **successful** run of the same dataset that started before this
one.

Successful matters: a failed run has no report to compare against, and skipping
over it is right. A comparison that reset itself every time an audit crashed
would be worse than no comparison, because the week it silently stopped
comparing is the week nobody notices.

Same dataset matters too — two datasets audited by the same server are not each
other's history. A dataset's first audit has no comparison at all, and the
report simply does not carry the field.

Nothing here can fail a run. A baseline that cannot be read is a comparison
nobody sees; the audit itself is unaffected.

## What it is not

**It is not a switch.** Unlike the agent, there is no egress decision here for
anybody to take: the comparison is derived from documents already stored on the
customer's own machine, and nothing new leaves the process. Making it optional
would only mean the browser and an assistant over MCP could disagree about
whether a run has one.

**It is not a rename detector.** A column renamed between two exports is a
resolved finding and a new one, not a moved one. Guessing that two differently
named columns are the same column is exactly the kind of inference that turns a
comparison into something a reader cannot check — and the table drift section
puts the rename in front of them instead, which is the fact they need.

**It does not compare arbitrary runs.** One run against the one before it, or
against a file you name. Choosing any two runs of a dataset out of the history
is a screen and an endpoint that nobody has asked for yet.
