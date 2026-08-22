# Billing warehouse catalog — tariff lifecycle

Extract from the warehouse catalog. This is the record of which reference codes
are open, and it is the only place the dates live: the `tariffs` table in the
export carries the rates and not the lifecycle.

It governs `meters.tariff_code`. A meter may only be placed on a code that was
open on the day it was commissioned, which is `meters.installed_on`. Nothing in
the export enforces that or records it, because the export has no lifecycle
column: a closed code is still a valid `tariff_code` and still appears in
`tariffs`, since the meters already on it are still billed on it.

| Code | Opened | Closed to new meters | Replaced by | Notes |
|---|---|---|---|---|
| `STD-A` | 2016-04-01 | **2024-06-30** | `STD-B` | Withdrawn under the 2024 price cap restructure. Existing meters continue to be billed on it, so it is still a valid tariff and still appears in the `tariffs` table. **No meter commissioned after the closing date may be placed on it** — one that is will be billed on a rate that no longer has a published cap, which is a compliance finding and not merely an error. |
| `STD-B` | 2024-07-01 | — | — | The replacement. Every domestic meter commissioned from 2024-07-01 goes here unless it is on Economy Seven. |
| `ECO-7` | 2011-10-01 | — | — | Economy Seven, dual rate. Open. |
| `COM-1` | 2016-04-01 | **2023-12-31** | `COM-2` | Withdrawn when the commercial book was repriced. Same rule as `STD-A`: existing meters stay on it, new ones may not be put on it. |
| `COM-2` | 2024-01-01 | — | — | The commercial replacement. Open. |

Closing dates are inclusive: a meter commissioned *on* 2024-06-30 may be on
`STD-A`, one commissioned on 2024-07-01 may not.
