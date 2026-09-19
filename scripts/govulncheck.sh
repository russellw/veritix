#!/usr/bin/env bash
#
# Run govulncheck and hold its findings against a written-down list of the ones
# already accounted for.
#
#   scripts/govulncheck.sh            # what `make audit` and CI both run
#
# govulncheck is one half of what stands in for a vendor/ directory here (the
# other is `go mod verify`), so switching it off is not available. What it
# cannot do is carry a note: it reports what the vulnerability database says,
# and the database is occasionally wrong about a module Veritix ships. This
# wrapper is where that note lives, and it is written so that an entry cannot
# outlive its reason -- a stale acceptance is worse than none, because it reads
# as a check that is running.
#
# Every accepted entry names the exact module version it was verified against,
# and the run fails if:
#
#   * any vulnerability reaches Veritix's own code and is not on the list;
#   * an accepted one stops being reported, so the entry is now dead wood;
#   * an accepted one is reported against a different version of the module,
#     so whatever was verified was verified about something else;
#   * an accepted one acquires a fixed version, so the advisory has been
#     corrected and the answer is an upgrade rather than a note.
#
# Adding an entry means reading the advisory and the module's source, and
# writing down what was actually checked. "It looks unreachable" is not it.

set -euo pipefail

# ── accepted findings ──────────────────────────────────────────────────────
#
# One line each: <GO id> <module>@<version> <one-line reason>.
#
# GO-2026-6452 -- panic via negative shared-string index in excelize.
#   The Go vulnerability database has no fixed version for this: the advisory's
#   affected range is "introduced at 0" with no end, so every release matches,
#   including the one carrying the fix. The fix is upstream PR #2331, commit
#   93f0b3ca, and it is *in* v2.11.0 -- `newInvalidSharedStringIndex` is at
#   errors.go:314 and the bounds check that calls it is at cell.go:627, where
#   `len(d.SI) > xlsxSI` used to let -1 through. Verified by hand as well as by
#   reading it: a workbook holding `<c t="s"><v>-1</v></c>` against a non-empty
#   shared-string table returns the error "invalid shared string index -1"
#   rather than panicking. So the finding is about the advisory's metadata and
#   not about the code Veritix ships. Remove this entry when the advisory gains
#   a fixed version, which the check below will insist on.
ACCEPTED=(
	"GO-2026-6452 github.com/xuri/excelize/v2@v2.11.0 advisory has no fixed version; the fix is in v2.11.0"
)

# ───────────────────────────────────────────────────────────────────────────

command -v jq >/dev/null 2>&1 || {
	echo "govulncheck.sh: jq is required" >&2
	exit 2
}
command -v govulncheck >/dev/null 2>&1 || {
	echo "govulncheck.sh: govulncheck is required:" >&2
	echo "  go install golang.org/x/vuln/cmd/govulncheck@latest" >&2
	exit 2
}

report=$(mktemp)
trap 'rm -f "$report"' EXIT

# govulncheck exits 3 when something reaches the code being scanned, which is
# the case this script exists to adjudicate, so its status is not the answer.
# Any other non-zero status is the scan itself failing and is fatal.
status=0
govulncheck -format json ./... >"$report" 2>/dev/null || status=$?
if [ "$status" -ne 0 ] && [ "$status" -ne 3 ]; then
	echo "govulncheck.sh: the scan failed (exit $status); running it directly:" >&2
	govulncheck ./... >&2 || true
	exit "$status"
fi

# A finding whose first trace frame names a function is one govulncheck traced
# into Veritix's own code. The rest are packages imported but not called and
# modules required but not imported, which it reports for information and does
# not fail on; this script takes the same line.
found=$(jq -r --slurp '
	.[] | select(.finding) | .finding
	| select(.trace[0].function != null)
	| [.osv, (.trace[0].module + "@" + .trace[0].version), (.fixed_version // "-")]
	| @tsv
' "$report" | sort -u)

fail=0
note() {
	echo "govulncheck.sh: $*" >&2
	fail=1
}

seen=""
while IFS=$'\t' read -r osv modver fixed; do
	[ -n "$osv" ] || continue
	seen="$seen $osv"
	entry=""
	for a in "${ACCEPTED[@]}"; do
		case "$a" in "$osv "*) entry="$a" ;; esac
	done
	if [ -z "$entry" ]; then
		note "$osv affects $modver and is not accounted for." \
			"Upgrade the module, or add an entry to ACCEPTED in this script saying why not."
		continue
	fi
	want=$(awk '{print $2}' <<<"$entry")
	if [ "$modver" != "$want" ]; then
		note "$osv is accepted against $want but is reported against $modver." \
			"Re-verify the finding against the version in use and update the entry."
	fi
	if [ "$fixed" != "-" ]; then
		note "$osv now has a fixed version ($fixed)." \
			"The advisory has been corrected: upgrade the module and delete the entry."
	fi
done <<<"$found"

for a in "${ACCEPTED[@]}"; do
	osv=$(awk '{print $1}' <<<"$a")
	case " $seen " in
	*" $osv "*) ;;
	*) note "$osv is accepted but no longer reported. Delete the entry." ;;
	esac
done

if [ "$fail" -ne 0 ]; then
	echo >&2
	govulncheck ./... >&2 || true
	exit 1
fi

n=$(printf '%s' "$found" | grep -c . || true)
if [ "$n" -gt 0 ]; then
	echo "govulncheck: clean, with $n accepted finding(s):"
	printf '  %s\n' "${ACCEPTED[@]}"
else
	echo "govulncheck: clean"
fi
