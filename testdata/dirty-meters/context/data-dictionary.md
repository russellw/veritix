# Metering data dictionary

Maintained by the Metering Data team. Definitions here are authoritative for
every export out of the billing warehouse. Where an export disagrees with this
page, the export is wrong.

Last reviewed: 2025-08-04.

## premises

| Column | Type | Definition |
|---|---|---|
| `upn` | integer | Unique Premises Number. Allocated centrally, never reused, never reissued after a demolition. |
| `address` | text | First line of the postal address as held by the supply-point registry. |
| `postcode` | text | UK postcode, uppercase, single space before the inward code. |
| `region` | text | Distribution region. One of `South`, `North`, `Scotland`, `Wales`. |

## meters

| Column | Type | Definition |
|---|---|---|
| `meter_id` | text | Meter serial as `MTR-` followed by four digits. |
| `site_ref` | text | **The premises this meter serves, written as the letters `UPN-` followed by that premises' `upn`.** It is not a foreign key in the export and joining it needs the prefix stripped: `UPN-4471` is premises `4471`. The prefix is a legacy of the 2019 migration and there is a ticket to remove it. |
| `installed_on` | date | Date the meter was commissioned on site. No reading can predate it. |
| `register_digits` | integer | How many digits the mechanical register displays. A register rolls over to zero when it passes its maximum, so a reading is never wider than this. |
| `tariff_code` | text | The tariff this meter is billed on. See `tariffs`, and see the warehouse catalog for which codes are still open to new meters. |
| `status` | text | Lifecycle state. **Exactly one of `active`, `inactive`, `removed`.** Nothing else is permitted: every downstream report filters on this column by name, so a state that is not on this list is silently excluded from billing, from the asset register and from the regulatory return. |

## readings

| Column | Type | Definition |
|---|---|---|
| `reading_id` | text | Reading identifier as `RDG-` followed by six digits. Unique. |
| `meter_id` | text | The meter read. Foreign key to `meters`. |
| `read_on` | date | Date the read was taken. ISO 8601. |
| `register_value` | integer | **The cumulative total shown on the register at the moment of the read.** It is the odometer, not the trip: it is *not* the consumption since the last read. Consumption is derived downstream by subtracting consecutive reads, so for a given meter this value must never be lower than the previous read's. A drop means a misread digit, a meter exchange recorded against the old serial, or two reads keyed in the wrong order — and it produces negative consumption in the billing run. |
| `source` | text | How the read was obtained: `A` actual, `E` estimated, `C` customer-supplied. |

## tariffs

| Column | Type | Definition |
|---|---|---|
| `tariff_code` | text | Tariff identifier. |
| `name` | text | Customer-facing name. |
| `unit_rate` | decimal | Pounds per kWh, excluding VAT. |
| `standing_charge` | decimal | Pounds per day, excluding VAT. |

Every tariff a meter is billed on must appear in this table, including tariffs
closed to new business: closing a tariff does not remove it, because meters
already on it keep being billed.
