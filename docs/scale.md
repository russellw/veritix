# Auditing something big

Both committed fixtures are tens of rows. Everything built for size — the
engine's memory limit and query timeout, DuckDB's spill to `temp_dir`, the
profiler's fan-out across columns, the anti-joins in `checks/relate.go` — had
therefore never run against data that would reach it, and neither had any
claim made about it. This is what happened when it did.

The short version: **a check that measures a share goes blind as the file
grows, and an audit that runs out of time drops what it could not measure and
reports the rest.** Both were true, both are fixed, and the fixes are the
reason this page exists rather than the timings.

## The fixture

`scripts/gen-dataset` writes a dataset and the manifest that describes it:

```sh
go run ./scripts/gen-dataset -out /var/tmp/vx-big -scale 1
./bin/veritix eval /var/tmp/vx-big
```

At scale 1 that is 2.0 GB across five files: 20M orders, 2M customers, 50k
products, a 24-row region table, and a 200-column metrics export — 22.25M rows
and 231 columns. `-scale 0.05` is a twentieth of the data, runs in a
minute, and finds the same defects (16 of the 17: the ragged rows are
planted one in a million and there are not a million, and the manifest leaves
them out rather than reporting a miss for a defect that is not there);
`-seed` makes a run repeatable on another machine.

It is a generator rather than a committed fixture because a two-gigabyte
fixture is not reviewable and does not belong in git history. The defects are
the same kinds `dirty-retail` carries — placeholders, a second date format,
duplicate rows, a broken reference, magic numbers, an empty column — planted at
one row in a few thousand rather than one row in nine. That ratio is the whole
experiment: **the defect is the same size and the file is not.**

The generator writes `veritix-manifest.yaml` from its own tallies, so
`veritix eval` scores the same run that is being timed. A scale test that only
measures seconds cannot tell a fast auditor from one that quietly stopped
looking, which is exactly the failure that turned up.

## What it costs

Measured on four cores and 30 GB of RAM, with `veritix eval` (no model), at
scale 1, on the shipped defaults, with `engine.temp_dir` pointed at real disk.

| stage | duration |
|---|---|
| discover | 0s |
| ingest | 1m 01s |
| profile | 13m 11s |
| checks | 13s |
| verify | 5s |
| **total** | **14m 30s** |

`17 of 17 planted defects found, 0 false positives`, which is the half of the
measurement that took the work below to reach. Peak resident memory 4.2 GiB;
3,353 seconds of CPU against 14.5 minutes of wall clock, a factor of 3.9 on
four cores, so the machine rather than the program is the constraint.

Before the last change on this page — the same dataset with its tables in
memory rather than in a DuckDB file — the same run was 22m 53s and 7.2 GiB.

**Profiling is the cost, and it is linear in cells rather than rows.**

| table | rows | columns | profiled in |
|---|---|---|---|
| orders.csv | 20M | 10 | 9m 19s |
| metrics.csv | 200k | 201 | 3m 01s |
| customers.csv | 2M | 12 | 50s |
| products.csv | 50k | 6 | 1.6s |
| regions.csv | 24 | 2 | 0.01s |

Ingest is 7% of the run: DuckDB reads 2 GB of CSV and writes its own storage in
a minute, and everything after that is measurement. A cell costs between 2.5
and 9 µs depending on what is in it — four to six full scans with regular
expressions and date parsing on every value — so the number to plan with is
**cells × 5 µs, divided by however many columns run at once** (eight, and not
configurable). A wide table is the shape to watch: 201 columns of 200k rows
cost three and a half times what 2M rows of twelve columns did, and were the
one table the move to a file did not speed up at all.

## What it found

### The audit stopped auditing and did not say so

At 20M rows every column of `orders.csv` exceeded the two-minute
`engine.query_timeout`. The profiler logged a warning per column, substituted a
stub, and carried on. A stub has no nulls, no placeholders and no type
violations, so the checks that read it found nothing, and the run finished in
11 minutes reporting **13 of 17 planted defects with zero false positives** —
a clean, confident report on a table nothing had looked at, twice as fast as a
correct one because the checks had nothing to read. The four it missed were all
in `orders.csv`, including a hundred orders pointing at a customer that does
not exist.

Three changes:

- **`column.not_profiled`** is a finding. A measurement that did not run has to
  appear in the report for the reason `rule.never_applied` does: silence means
  either "this column is fine" or "nothing looked at it", and the second is
  dangerous exactly where it happens. It carries no evidence query — the
  measurement is what failed — and no engine error text, because a DuckDB error
  quotes the value that caused it.
- **No other column check runs on an unmeasured column**, and
  `table.no_candidate_key` stays quiet when any column in the table is
  unmeasured. Both would otherwise be claims drawn from measurements that do
  not exist, and "no column identifies a row" is the one that reads as a defect
  in the data.
- **`engine.query_timeout` now defaults to 30 minutes**, and a second limit,
  `engine.agent_query_timeout`, defaults to 2 minutes and bounds a statement
  the model wrote. That is where the short limit always belonged: a measurement
  Veritix wrote costs what the data costs, while a model's SQL is unreviewed,
  arrives up to forty times a run, and is the only SQL in the process nobody
  chose. `agent.Options.UseEngineLimits` applies both, because four entry
  points were each copying `MaxRows` and a fifth line in four places is a line
  that goes missing from one.

### Two checks measured a share, so they went blind

`column.missing_values` ignored a column less than 5% missing. `N/A` in two of
nine rows is 22% and fires; thirteen of two million is 0.0006% and does not.
The share is not the property that matters — a placeholder defeats every null
check downstream whatever its share, and the bigger the file the less likely
anyone notices by eye. Placeholders now count at any rate; genuine blanks keep
the 5% floor, because a column that is 3% empty really is unremarkable.

That split needed one more distinction. A *text* placeholder is never a
measurement, but `-1` and `999` sometimes are, and reporting every column that
contains a 999 would report every column of two million numbers. A magic number
counts when it stands out from the column around it: more repeated than any
real value (`standsOut`), or negative where nothing real is
(`wrongSideOfZero`, against `NumericStats.MinReal`, which excludes the magic
numbers from their own comparison — `Min` is -999 precisely because -999 is in
the column). `-999` among credit limits of 1000 and up qualifies on the second
even when a popular default beats it on the first.

Sign rather than distance, and that is the part worth keeping. The obvious rule
— the placeholder sits outside the range of every real value — reports the
largest value of every uniform column, because in a column of two hundred
thousand random numbers something has to be the maximum and 999999 is as good a
candidate as any. Sign is a boundary business data respects: a credit limit, a
quantity, a weight, a price is not negative, so a negative one is announcing
itself. The large positive magic numbers are left to `standsOut` and to
`column.numeric_outliers`, which is what a value genuinely far above the data
trips.

`column.mixed_date_formats` dismissed any format accounting for under 2% of the
column, on the grounds that a tiny second format is usually one format matched
by two patterns rather than a real mixture. That reasoning is right and the
test was not: `05/06/2019` parses day-first and month-first alike, so a column
written entirely day-first reports two formats and the 2% floor is what hid the
artifact. It also hid the real thing, since two thousand rows written the other
way round in a two-million-row export is 0.1%. The profiler now measures
`FormatCount.Exclusive` — how many values a format reads that the *leading*
format cannot — and a format explaining nothing new is not a second format at
any size. The leading format's probe goes first in the SQL so DuckDB's short
circuit evaluates the rest only on the rows that failed it, which makes the
extra pass cost about one `strptime` per row.

### A finding's count and its evidence were different numbers

`column.missing_values` counted every sentinel and demonstrated itself with a
query matching only the textual ones. `finding.Set.Verify` re-runs every
evidence query, and when the two disagree it trusts the engine and **silently
corrects the count** — so the title kept saying one number while the finding
carried another, which is precisely the failure `record_finding` refuses to
allow the model. On the fixture it showed up as a finding on a column of unique
order ids being dropped for not reproducing at all. The check now counts what
its own predicate matches, by construction.

### Holding the dataset in memory was the slow way to do it

Every entry point but the CLI already passed `--database`: the HTTP API and the
MCP server keep each run's DuckDB file so the per-finding rows endpoint can
reopen it afterwards. The CLI held its tables in memory, and the obvious
reading of that is a trade — memory for speed, since nothing is written to
disk.

It is not a trade. On the same 400 MB dataset, same machine, back to back:

| | wall clock | peak memory |
|---|---|---|
| in memory | 283 s | 1.67 GiB |
| DuckDB file | 182 s | 1.06 GiB |

**Faster and smaller both**, because DuckDB's persistent storage is compressed
where its in-memory tables are not: the file is 124 MB for 400 MB of CSV, and a
scan that reads a third as many bytes finishes sooner. There was nothing to
offer the caller a choice about, so `audit.Run` no longer offers one — a run
given no `DatabasePath` takes a temporary directory, puts the file there, and
`Result.Close` removes it. `--database` keeps its old meaning: name a file and
it outlives the run.

At 2 GB the same change took the run from 22m 53s and 7.2 GiB to 14m 30s and
4.2 GiB — a minute of that moved from profiling into ingest, which now writes
DuckDB's storage as well as reading the CSV, and the rest is scans reading
fewer bytes.

The directory is `engine.temp_dir` when one is set and the system temp
directory otherwise, which on many Linux boxes is a tmpfs — the same run with
the file on a tmpfs measured 1.15 GiB rather than 1.06, because those pages are
RAM. It costs the size of the file, not the benefit, but on a dataset large
enough to matter that is the setting to point somewhere real.

### Ten and a half minutes of silence

The pipeline logged "loaded dataset" and then nothing at all until the checks
finished, which on the first full-scale run was ten and a half minutes. The
browser's progress stream is those same log lines, so it showed the same
nothing. `audit.Run` now announces every stage on entry and reports
its duration on exit, and `profile.Run` logs each table as it finishes with its
column count and how long it took — which is also what made the timings on this
page possible to take.

## What is still true

- **`engine.memory_limit` bounds the working set, not the dataset**, now that
  every run holds its tables in a file. The shipped Kubernetes base sets 2GB
  against a 3Gi container, and that is not a two-gigabyte ceiling on the CSV.
  What it does mean is that a run needs scratch space: about a third of the
  dataset's size, under `engine.temp_dir` when one is set, which in the
  container is the `emptyDir` at `/tmp`.
- **Nothing here was measured with a model.** The generated dataset carries one
  agent target — a `ship_date` five days before its `order_date`, which every
  value in both columns is individually fine about — but a 24-step agent run
  over a table this size is a separate measurement, and
  `docs/local-model.md` is where that kind of number goes.
- **The wide table is the shape to watch.** 201 columns of 200k rows cost more
  than 2M rows of twelve columns. Profiling parallelism is fixed at eight
  columns at a time and is not configurable; on a machine with more cores than
  this one that is the first number to want.
