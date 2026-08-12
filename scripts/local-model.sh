#!/usr/bin/env bash
# Drive the agentic auditor against a local model, by hand.
#
# This is deliberately not a `make` target and never runs in CI: a real local
# model takes twenty minutes and gives a different answer every time, while
# `make e2e` drives a scripted stand-in in seconds. Those are different
# activities. See docs/local-model.md, which this script is the executable form
# of — the flags, the preflight and the checks afterwards all come from there.
#
# Usage:
#   scripts/local-model.sh                 # probe, audit, then check the trace
#   scripts/local-model.sh --probe         # just the probe: is this thing usable
#   scripts/local-model.sh --serve         # serve the UI wired to the model
#   scripts/local-model.sh -- --rules x.yaml --format json
#                                          # anything after -- goes to veritix
#
# Overridable:
#   MODEL       default qwen3:4b-instruct-2507-q4_K_M
#   BASE_URL    default http://localhost:11434/v1
#   DATASET     default testdata/dirty-retail
#   MAX_STEPS   default 24
#   TIMEOUT     default 30m   (one model call; the product default of 10m is
#                              sized for a cloud endpoint, not for this)
#   OUT_DIR     default ./local-runs
#   ADDR        default 127.0.0.1:8080   (--serve only)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

MODEL="${MODEL:-qwen3:4b-instruct-2507-q4_K_M}"
BASE_URL="${BASE_URL:-http://localhost:11434/v1}"
DATASET="${DATASET:-testdata/dirty-retail}"
# 12 was not enough twice over: a small model spends its first several steps
# working through list_tables and describe_table, so a short budget stops it
# while it is still orienting and records nothing. Size the budget for the
# model rather than for the dataset — docs/local-model.md, "Budget".
MAX_STEPS="${MAX_STEPS:-24}"
# Ten minutes is the product default and is right for a cloud endpoint. Here
# generation slows as the context fills, so a late step can outrun it, and the
# run then ends on an error instead of a report.
TIMEOUT="${TIMEOUT:-30m}"
OUT_DIR="${OUT_DIR:-$repo_root/local-runs}"
ADDR="${ADDR:-127.0.0.1:8080}"

mode=audit
extra=()
while [ $# -gt 0 ]; do
	case "$1" in
	--probe) mode=probe ;;
	--serve) mode=serve ;;
	-h | --help)
		awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "${BASH_SOURCE[0]}"
		exit 0
		;;
	--)
		shift
		extra=("$@")
		break
		;;
	*)
		echo "unknown argument: $1 (try --help)" >&2
		exit 2
		;;
	esac
	shift
done

say() { printf '\n\033[1m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[33mwarning: %s\033[0m\n' "$*" >&2; }

# ── preflight ──────────────────────────────────────────────────────────────
#
# Twenty seconds of checking beats twenty minutes of a run that was never going
# to work. Each of these corresponds to a specific way the first attempt failed.

say "Checking the model server at $BASE_URL"

# /models is a convenience, not the gate: the dialect's implementations disagree
# about which endpoints they bother with, and the probe below is what actually
# decides whether a run is worth starting.
if models=$(curl -fsS --max-time 5 "$BASE_URL/models" 2>/dev/null); then
	if ! jq -e --arg m "$MODEL" 'any(.data[]?; .id == $m)' <<<"$models" >/dev/null 2>&1; then
		warn "the server does not list a model called '$MODEL'"
		echo "  it offers: $(jq -r '[.data[]?.id] | join(", ")' <<<"$models")" >&2
		echo "  pull it with: ollama pull $MODEL" >&2
	else
		echo "$MODEL is available"
	fi
else
	echo "the server does not answer /models; the probe will settle it"
fi

# The two-tool probe from docs/local-model.md. What it answers is not "is the
# model clever" but "does this server implement tool calling at all", which is
# the failure that otherwise shows up as an agent that talks in prose.
say "Probing tool calling (this also loads the weights)"

probe_body=$(
	jq -n --arg m "$MODEL" '{
    model: $m, max_tokens: 128, temperature: 0,
    messages: [
      {role: "system", content: "You inspect datasets. Use a tool. Never answer in prose."},
      {role: "user", content: "How many rows are in the orders table?"}
    ],
    tools: [
      {type: "function", function: {name: "list_tables",
        description: "List the tables in the dataset.",
        parameters: {type: "object", properties: {}}}},
      {type: "function", function: {name: "run_query",
        description: "Run one SELECT and return the result.",
        parameters: {type: "object", required: ["sql"],
          properties: {sql: {type: "string", description: "the SELECT to run"}}}}}
    ]
  }'
)

probe_start=$(date +%s)
probe=$(curl -fsS --max-time 300 "$BASE_URL/chat/completions" \
	-H 'Content-Type: application/json' -d "$probe_body") || {
	cat >&2 <<EOF
The probe failed: $BASE_URL/chat/completions did not answer a two-tool payload.

If nothing is listening, start Ollama with the settings that are not optional:

  OLLAMA_CONTEXT_LENGTH=32768 OLLAMA_NO_CLOUD=1 OLLAMA_KEEP_ALIVE=60m \\
    OLLAMA_FLASH_ATTENTION=1 ollama serve

OLLAMA_CONTEXT_LENGTH is the one that will cost you a day: with no GPU Ollama
picks 4096, the first agent prompt is ~4080, and the system prompt is then
discarded from the front mid-run. That reads as a stupid model rather than a
truncated one.

If something is listening, it answered an error — run the same request by hand
to see what it said.
EOF
	exit 1
}
probe_secs=$(($(date +%s) - probe_start))

if [ "$(jq -r '.choices[0].message.tool_calls | length // 0' <<<"$probe")" -gt 0 ]; then
	echo "tool calling works: $(jq -r '.choices[0].message.tool_calls[0].function.name' <<<"$probe") in ${probe_secs}s"
else
	warn "the model answered in prose rather than calling a tool"
	jq -r '.choices[0].message.content // "(no content)"' <<<"$probe" | head -5 >&2
	echo "  a run may still work, but a non-thinking instruct model is the one to want" >&2
fi

# Ollama reports the context window of a loaded model on its native API, which
# is the only way to see from out here whether OLLAMA_CONTEXT_LENGTH was set.
native="${BASE_URL%/v1}"
ctx=$(curl -fsS --max-time 5 "$native/api/ps" 2>/dev/null |
	jq -r '.models[0].context_length // empty' 2>/dev/null || true)
if [ -n "$ctx" ]; then
	if [ "$ctx" -lt 8192 ]; then
		warn "the loaded context window is $ctx tokens; the first agent prompt is ~4080"
		echo "  restart the server with OLLAMA_CONTEXT_LENGTH=32768 or the system prompt" >&2
		echo "  will be discarded mid-run" >&2
	else
		echo "context window: $ctx tokens"
	fi
fi

if [ "$mode" = probe ]; then
	exit 0
fi

# ── the run ────────────────────────────────────────────────────────────────

say "Building"
make build >/dev/null

if [ "$mode" = serve ]; then
	say "Serving on http://$ADDR with the agent available"
	echo "The agent is per-run: tick it when starting an audit, or POST /runs with"
	echo '{"agent": true}. The trace lands at /api/v1/runs/<id>/trace.'
	exec env \
		VERITIX_LLM_PROVIDER=openai-compatible \
		VERITIX_LLM_BASE_URL="$BASE_URL" \
		VERITIX_LLM_MODEL="$MODEL" \
		VERITIX_LLM_MAX_STEPS="$MAX_STEPS" \
		VERITIX_LLM_REQUEST_TIMEOUT="$TIMEOUT" \
		./bin/veritix serve --addr "$ADDR"
fi

mkdir -p "$OUT_DIR"
stamp="$(date +%Y%m%d-%H%M%S)"
slug="$(printf '%s' "$MODEL" | tr -c 'A-Za-z0-9._-' '-')"
trace="$OUT_DIR/$stamp-$slug.trace.json"
log="$OUT_DIR/$stamp-$slug.log"
report="$OUT_DIR/$stamp-$slug.report.txt"

say "Auditing $DATASET with $MODEL (max $MAX_STEPS steps, ${TIMEOUT}/call)"
echo "trace:  $trace"
echo "log:    $log"

# --log-level debug so that tool calls appear as they happen; the trace only
# lands at the end, and a long run with no output looks like a hang.
started=$(date +%s)
status=0
# There is no --llm-request-timeout flag; the setting is config and env only.
VERITIX_LLM_REQUEST_TIMEOUT="$TIMEOUT" \
	./bin/veritix audit "$DATASET" \
	--llm openai-compatible \
	--llm-base-url "$BASE_URL" \
	--llm-model "$MODEL" \
	--llm-max-steps "$MAX_STEPS" \
	--trace-out "$trace" \
	--log-level debug \
	"${extra[@]}" >"$report" 2> >(tee "$log" >&2) || status=$?
elapsed=$(($(date +%s) - started))

# --fail-on is not passed, so a non-zero exit is a real failure rather than
# findings having been reported.
if [ "$status" -ne 0 ]; then
	echo "veritix exited $status after ${elapsed}s; see $log" >&2
	exit "$status"
fi

say "Report ($((elapsed / 60))m $((elapsed % 60))s)"
sed -n '1,8p' "$report"
echo "  full report: $report"

# ── what the model actually did ────────────────────────────────────────────

if [ -s "$trace" ]; then
	say "Trace"
	jq -r '
    "steps:        \(.steps | length) of \(.max_steps)",
    "tool calls:   \([.steps[].calls // [] | length] | add // 0)"
      + " (\([.steps[].calls // [] | .[] | select(.is_error)] | length) refused)",
    "findings:     \(.findings) recorded, \(.not_reproduced) not reproduced",
    "stopped:      \(.stopped)" + (if .error != null then " — \(.error)" else "" end),
    "tokens:       \(.usage.input_tokens) in, \(.usage.output_tokens) out",
    "withheld:     \(.redaction.shaped) shaped, \(.redaction.masked) masked,"
      + " \(.redaction.truncated) truncated, over \(.redaction.bytes) bytes sealed",
    "values sent:  \(.values_allowed)"
  ' "$trace"
fi

# The egress check from docs/local-model.md, against a real model rather than
# llmtest's scripted one. These are verbatim contents of the fixture, and the
# same list internal/report's tests assert on; none of them may appear in a
# payload that left the process.
if [ -s "$trace" ] && [ "$DATASET" = "testdata/dirty-retail" ]; then
	say "Egress check"
	leaked=0
	for raw in \
		"CUS-000001" "CUS-000005" "CUS-999999" \
		"alice@example.com" "carol@example.com" \
		"Alice Smith" "Frank Green" \
		"Zürich" "München" "Montréal" \
		"Doohickey" "Widget" \
		"Quarterly Sales Report"; do
		n=$(grep -Fco -- "$raw" "$trace" || true)
		if [ "$n" -gt 0 ]; then
			echo "LEAK: $raw appears $n time(s) in the trace" >&2
			leaked=1
		fi
	done
	if [ "$leaked" -ne 0 ]; then
		echo "the guard let a cell value through — that is a bug, not a model problem" >&2
		exit 1
	fi
	echo "no fixture cell value appears in anything sent to the model"
fi
