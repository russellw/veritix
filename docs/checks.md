# Every check, and what each one is for

This is the full list of what Veritix reports **with no model configured** —
`llm.provider: none`, which is the default and is a complete auditor on its
own. Everything here is deterministic: the same dataset produces the same
findings, and every one of them carries the SQL that demonstrates it.

There are **44 built-in rules**, plus three forms that only appear once a rules
file is in play. An agentic run (`--llm anthropic`, or `"agent": true` over
HTTP) adds findings on top of these; it never removes one. See
[docs/eval.md](eval.md) for what a model adds and how that is measured.

## How to read the list

**A rule id names a class of defect, not a place.** `column.mixed_date_formats`
is the rule; the finding's location says which column. The id is the stable
half — it is what `--fail-on`, the eval manifests, and the run-over-run
comparison all key on.

**Severity is what the rule means, not how many rows it hit.** An error is
something that makes a downstream number wrong. A warning is something that
will make one wrong given the right query. Info is worth knowing. The
severities below are what the check assigns; a customer rule assigns its own.

**Every finding is re-run before it is reported.** A finding with a
`CountQuery` goes through `finding.Set.Verify`, which executes it again and
drops the finding if it no longer reproduces. The handful of rules with no
count query are noted as such below — in each case there is no number to
reproduce, because the measurement itself is what failed or the observation was
made while reading the file rather than by querying it.

**Findings say what breaks downstream.** Not "this column has two date
formats", but "some of these dates will be read as the wrong day and nothing
will error". That is a deliberate house style, and it is why the detail text is
longer than a lint message.

---

## Column checks

Run against every column of every table, in the order listed. `internal/checks/column.go`.

`column.not_profiled` is the exception to the ordering: when it fires, **no
other column check runs on that column at all**. Every other check reads
measurements that column does not have, and a zero count reads exactly like a
clean column.

| Rule | Severity | What it reports |
|---|---|---|
| `column.empty` | warning | The column has rows but not one populated value — every cell is null, blank, or a placeholder. A shape with nothing in it, usually a field the export forgot to fill. |
| `column.not_profiled` | warning | The measurement did not run, so nothing in the report is a claim about this column. Almost always a per-query timeout on a very large table; the remedy names `engine.query_timeout`. Carries no count query — the measurement is what failed. |
| `column.missing_values` | info, or warning at ≥50% | Rows with no usable value, counting **placeholders as missing**: `N/A`, `-`, `unknown` and their kin defeat every null check downstream, so they are counted alongside genuine nulls and blanks. |
| `column.type_violation` | error | The column is clearly one type, and some values are not it and are not a recognized way of writing "missing" either. An import that types this column turns each of them into a null and reports nothing. |
| `column.mostly_typed` | warning | At least 60% of a text column parses as a number or a date, but not enough of it to treat the column as one. The signature of a numeric or temporal field that has been contaminated: it cannot be summed, sorted, or compared as its real type anywhere downstream. |
| `column.mixed_date_formats` | error | The column is written in more than one date format. A single-format reader will either fail on the others or, worse, parse them into the wrong dates. |
| `column.ambiguous_dates` | error | Values that parse under both `DD/MM/YYYY` and `MM/DD/YYYY` and **mean a different day under each**. Nothing in the value says which was intended, so whichever convention a reader assumes, some of these are wrong and nothing raises an error. |
| `column.implausible_dates` | warning | Dates before 1900 or more than a century ahead: an escaped placeholder, an epoch-zero default, or a parsing accident. They drag every minimum, maximum and range computed on the column. No count query — the range is the evidence. |
| `column.future_dates` | warning | Dates after the audit ran, in a column that records something that has already happened. A column whose name says it holds something scheduled is exempt. No count query. |
| `column.whitespace_padding` | warning | Leading or trailing spaces. Invisible on screen and not to a comparison: `" ACME"` and `"ACME"` are different values to a join, a `GROUP BY`, and a lookup. |
| `column.case_variants` | warning | The same value written several ways once case and surrounding spaces are ignored — `Active`, `active`, `ACTIVE `. Grouping splits one category across several, so counts per category are wrong without anything looking wrong. Only fires on columns behaving like a category, never on free text. |
| `column.numeric_outliers` | info | Values more than the profiler's sigma threshold from the mean. The usual signature of a units mix-up (pence against pounds), a misplaced decimal point, or a placeholder never meant to be read as a quantity. |
| `column.unexpected_negative` | warning | Negative values in a column whose *name* rules them out — `quantity`, `price`, `weight`, `total` and similar. Anything explicitly signed (`balance`, `delta`, `net`, `margin`, …) is exempt. Usually refunds recorded in the same column as the original, which any total silently nets off. |
| `column.constant` | info | The same value in every row of a table with at least five. A column that never varies carries no information, and is often a filter that was applied when the export was produced — which means the file is a subset rather than the whole. No count query. |
| `column.duplicate_header` | warning | Two columns in the file share a header name, so the loader had to rename the second. Every other tool does something different: some take the first, some the last, some fail. |

### How the placeholder half of `column.missing_values` decides

Worth stating on its own, because it is the check most likely to look
inconsistent from the outside.

Below 5% missing the finding is suppressed — *unless* there are placeholders,
in which case it fires at any rate. A few blanks in a large column is not news;
thirteen `N/A`s in two million rows defeat a null check exactly as thoroughly
as two in nine, and nobody is going to notice by eye.

**Every text placeholder counts.** `"n/a"` is never a quantity.

**A numeric placeholder counts only where it stands out**, because `-1` and
`999` are sometimes measurements. Two tests, either of which is enough:

- `standsOut` — the magic number is more repeated than any real value in the
  column. `-999` occurring five hundred times where nothing real occurs more
  than sixty is not a coincidence, and that holds at any size where a share of
  the column does not.
- `wrongSideOfZero` — the magic number is negative in a column where nothing
  real is. `-999` among credit limits announces itself however rarely it
  occurs. Sign rather than distance, because "just past the maximum" is where
  the largest value of every uniform column lives.

The finding's count is built from exactly the values these tests admitted, so
the number in the title is the number its own evidence query produces.

---

## Table checks

Run once per file. `internal/checks/table.go`.

| Rule | Severity | What it reports |
|---|---|---|
| `table.empty` | warning | A header and no data rows. Usually a failed extract rather than a genuine absence of data, and indistinguishable from "nothing happened" in any report built from it. |
| `table.unreadable_rows` | error | Rows the parser could not read at all, because they have a different number of fields than the header declares. The most under-appreciated defect in a data file: the rows are not wrong, they are **absent**, so every count and total is quietly short and nothing downstream can detect a row that was never loaded. |
| `table.duplicate_rows` | error | Rows identical in every column. The count is the **surplus** — how many rows would disappear on de-duplication — because that is the figure that matters to anyone reconciling a total. |
| `table.no_candidate_key` | info | No single column holds a distinct, complete value for every row, so there is no way to refer to a particular record. Goes quiet when any column in the table is unprofiled: "no column identifies a row" is a claim about every column, and one of them was not measured. |

---

## Cross-file relationship checks

Run once over the whole dataset, after every table is profiled.
`internal/checks/relate.go`. This is where the interesting defects live — each
file usually looks fine on its own, and what breaks is the join between them.

| Rule | Severity | What it reports |
|---|---|---|
| `key.duplicate_values` | error | A column that looks like an identifier and repeats a value. Everything that joins to it multiplies rows. |
| `reference.orphan_values` | error | Values in one file with no matching row in the file they evidently reference. Every one of those rows vanishes from an inner join and appears as a blank in an outer one, so any figure grouped by the parent silently omits them. |
| `domain.inconsistent_values` | warning | The same categorical column in two files, holding different sets of values. Either one file has values the other is missing, or the same category is spelled differently in each — both of which split a report's rows in ways nobody notices. |

### How a relationship is inferred

Nothing declares a foreign key in a folder of CSVs, so Veritix proposes them
and then measures whether they hold.

A column is a **candidate key** if at least 90% of its populated values are
distinct — or 50%, if it is named as its own table's identifier. That second
clause is a false positive paid for once: requiring near-perfect uniqueness
disqualified `customers.customer_id` because it contained a duplicate, and hid
both that duplicate and the broken reference pointing at it. The duplicate is a
defect in the key, not evidence that the column was never a key.

A reference is reported when the columns are **named** as a reference, or when
at least 85% of the child's values are present in the parent. Comparison is on
the normalized value, so a reference is never reported as broken purely because
of casing or padding — that is `column.whitespace_padding`'s or
`column.case_variants`' finding, with its own remedy. Below 20% containment
nothing is reported even with matching names: two columns that mismatch almost
entirely are not the same thing, and calling every row an orphan is noise.

Two exclusions worth knowing:

- **A column named as its own table's identifier is not a foreign key into
  anything.** `order_id` inside `orders.csv` identifies orders; it does not
  reference them.
- **`domain.inconsistent_values` skips identifiers**, which the reference check
  can say something far more useful about than "these two lists differ", and
  skips anything with more than 50 distinct values, which is not a category.

---

## Structural observations

These are made while *reading* the file, before anything is profiled — by
`internal/source` for CSV and Excel, and by `internal/ingest` on load. They are
promoted to findings by `checkStructure`, which is where their severity is
assigned (`internal/checks/table.go`, `structuralSeverity`). None carries a
count query: the observation was made by the reader, not by a query that could
be run again.

An observation with no entry in that table is reported at info rather than
dropped.

### CSV

| Rule | Severity | What it reports |
|---|---|---|
| `csv.delimiter_disagreement` | error | Automatic detection chose one delimiter and Veritix reads the file with another, because the other actually splits it. DuckDB's sniffer once chose `|` for a comma-separated file with ragged rows — a character appearing nowhere yields a perfectly consistent one column per row, and the file would have loaded as a single column with every column check passing vacuously. |
| `csv.inconsistent_width` | error | Fewer than 95% of the sampled lines have the expected number of fields. Usually an unquoted separator inside a value. |
| `csv.width_disagreement` | error | The header declares one number of columns and the data implies another. The header wins — it is the file's own statement of its width — and the rows that do not fit are reported rather than allowed to widen the schema. |
| `csv.dialect_undetectable` | error | The file is too irregular to describe automatically. It is read as standard comma-style quoting with a header row, and this says so. |
| `csv.header_duplicate` | warning | One header name used by several columns. An import silently renames all but the first. |
| `csv.header_blank` | warning | A column with no name. |
| `csv.header_whitespace` | warning | A header name with leading or trailing whitespace, which will not match a lookup on the trimmed name. |
| `csv.encoding_not_utf8` | warning | The file is not valid UTF-8 and is being read as Latin-1. Accented and non-Latin characters may be wrong, which makes identical values look distinct — and then a de-duplication check reports one person as two. |
| `csv.no_header` | info | No header row detected, so columns are identified by position and a column added or reordered upstream goes unnoticed. |
| `csv.bom` | info | A byte-order mark, stripped rather than treated as data. |
| `csv.delimiter_ambiguous` | info | The extension implies one delimiter and another also parses. The extension wins, and this records the ambiguity. |

### Excel

| Rule | Severity | What it reports |
|---|---|---|
| `excel.hidden_rows` | error | Data rows hidden in the sheet. They are still in the file and still counted by anything that reads it, so what is on screen is not what is in the data. |
| `excel.formula_errors` | error | Cells holding `#REF!`, `#DIV/0!` and friends. Anything importing the file reads the error text as ordinary data. |
| `excel.sheet_unreadable` | error | A worksheet that could not be read at all. |
| `excel.hidden_sheet` | warning | A hidden worksheet, invisible to somebody opening the workbook — confirm whether it belongs to the dataset. |
| `excel.merged_cells` | warning | A merged cell holds one value but covers several rows or columns, so every row but the first reads as empty. |
| `excel.stacked_tables` | warning | Blank rows inside the data, which usually means more than one table on a single sheet. |
| `excel.ragged_rows` | warning | Rows wider or narrower than the header row. |
| `excel.header_blank` | warning | A column with no name. |
| `excel.header_offset` | info | The header is not on row 1. A tool assuming it is would read a title as the column names. |
| `excel.title_row` | info | A non-blank row above the header, skipped as a title rather than read as data. |

### Ingest

| Rule | Severity | What it reports |
|---|---|---|
| `ingest.no_rows` | warning | The file loaded with a header and no data rows. |

`ingest.rejected_rows` is an observation and deliberately **not** promoted to a
finding of its own: `checkUnreadableRows` gives the same rows the fuller
`table.unreadable_rows` finding, with the offending line numbers, the parser's
reason, and a count that goes through verification.

---

## Customer rules

Only present when a rules file is loaded — `--rules yours.yaml` on the command
line, or a proposal somebody accepted, which lands in
`<DataDir>/datasets/<id>/rules.yaml` and is loaded on every later audit of that
dataset. The two are additive. See [docs/rules-proposal.md](rules-proposal.md).

| Rule | Severity | What it reports |
|---|---|---|
| `rule.<id>` | the rule's own, defaulting to error | Rows breaking a customer-authored expectation. `<id>` is the rule's own name, so a rule named `status_is_known` reports as `rule.` followed by that name, across every table it applies to. An omitted severity means error: somebody wrote the rule because breaking it matters. |
| `rule.never_applied` | warning | The rule matched no table or column of that name, so it **did not run and protected nothing**. Usually a rename upstream or a typo in the rule file. This exists because silence from a rule is ambiguous — it means either "your data is fine" or "this never ran", and the second is dangerous precisely when somebody is relying on it. |
| `rule.invalid` | warning | The rule produced SQL the engine rejected. A defect in the rule, reported rather than allowed to pass quietly. |

The expectations a rule can assert are `not_null`, `unique`, `positive`,
`non_negative`, `one_of`, `matches`, `range`, `not_future`, `references` and
`sql`. `allow_missing` exempts absent values, counting a placeholder such as
`N/A` as absent — somebody writing it means "where there is no value", not
"where the cell is empty but not where a person typed something into it".

An `sql` rule runs once per table whatever column it names, because its clause
is about a row; naming a column is optional and moves the finding's location to
that column, which is where a person looks for it. A named column matching
nothing leaves the rule unapplied, so a typo surfaces as `rule.never_applied`
exactly as it does for every other expectation.

---

## What is not on this list

**Agent findings.** With a provider configured, the model records findings of
its own, each verified by the engine before it reaches the report. They carry
model-authored rule slugs and are marked with a different origin. Nothing the
model does can suppress a check above.

**The comparison.** `--baseline`, and the `comparison` section every audit over
HTTP or MCP carries, is a diff between two reports rather than a check. New,
worsened, resolved, improved, unchanged, plus the volume and schema drift no
check can see. [docs/comparison.md](comparison.md) is the whole of it.

**Profile measurements.** Row counts, distinct counts, inferred types, shapes
and top values are evidence rather than findings. They are in the report and in
the API's document; they are not defects.

## Keeping this list honest

`internal/checks/docs_test.go` scans the source for every rule id a finding can
carry and fails if one of them is missing from this file — or if this file
names one that no longer exists. A reference list that drifts from the code is
worse than no reference list, because it is believed.
