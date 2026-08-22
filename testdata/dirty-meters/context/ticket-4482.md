# INC-4482 — "Estimated reads are being billed as actual"

**Status:** closed — could not reproduce
**Raised by:** Billing Operations
**Closed:** 2025-06-11

## Reported

Billing Operations reported that estimated reads appeared to be flowing into
the billing run marked as actual, and asked for the `source` column on
`readings` to be audited.

## Investigation

The export was checked and `source` was found to carry `E` on every read the
meter operator flagged as estimated. No read was found where `source` said `A`
and the operator's own record said otherwise. The reconciliation the reporter
was looking at joins on the wrong date column, which is a defect in that
report and not in this export.

## Outcome

No change to the export. The `source` column is correct as it stands and
should not be rewritten. Reopened only if a specific read can be named.
