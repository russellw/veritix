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
#   scripts/local-model.sh                 # start the model, probe, audit, check
#   scripts/local-model.sh --probe         # just the probe: is this thing usable
#   scripts/local-model.sh --serve         # serve the UI wired to the model
#   scripts/local-model.sh -- --rules x.yaml --format json
#                                          # anything after -- goes to veritix
#
# The default model is gpt-oss-120b under llama.cpp, because it is the only one
# measured here that does the interesting half of the job: it reaches for the
# check tools unprompted and records the orphaned reference relate.go does not
# propose. It is 63GB against 30GB of RAM, so it is served paged from disk by
# ~/big-local-llms/scripts/serve-prefetch.sh — and if nothing is listening at
# BASE_URL this script starts one and stops it again when the run ends. A server
# that was already up is left alone and left running, which is how to keep one
# warm across several runs and skip the load each time.
#
# A small model still runs, and is the cheap way to exercise a change to the
# loop rather than to the auditing:
#
#   BASE_URL=http://localhost:11434/v1 MODEL=qwen3:4b-instruct-2507-q4_K_M \
#     EFFORT=none TIMEOUT=30m scripts/local-model.sh
#
# Overridable:
#   MODEL       default gpt-oss-120b  (also the alias a server started here is
#                             given, so what is asked for and what is served
#                             cannot disagree)
#   MODEL_GGUF  default ~/big-local-llms/models/gpt-oss-120b-MXFP4.gguf
#   SERVE_SCRIPT default ~/big-local-llms/scripts/serve-prefetch.sh
#   BASE_URL    default http://127.0.0.1:11500/v1  (llama.cpp; Ollama on 11434
#                             works too, and the preflight adapts its advice to
#                             whichever answers)
#   DATASET     default testdata/dirty-retail
#   MAX_STEPS   default 24
#   EFFORT      default low   (gpt-oss quietly ignores anything that is not
#                              low/medium/high; `none` is for a hybrid Qwen and
#                              is inert here)
#   TIMEOUT     default 60m   (one model call; the first step of a paged run is
#                              nearly half the wall clock)
#   PROBE_TIMEOUT default 900 (seconds for the probe; a model paging its
#                              weights from disk needs minutes, not seconds)
#   START_TIMEOUT default 600 (seconds to wait for a server started here)
#   CONTEXT     default auto  (serve $DATASET/context over MCP when that
#                             directory exists, so a fixture whose defects are
#                             not in the data gets the documents that explain
#                             them; `off` is the unaided control, or name a
#                             directory of documents of your own)
#   OUT_DIR     default ./local-runs
#   ADDR        default 127.0.0.1:8080   (--serve only)
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# Parameter count does not predict whether a model can do this job, but nothing
# smaller than this has done it: the 4B uses the check tools and finds one thing,
# the 35B answered three whole runs with run_sql and never recorded anything, and
# gpt-oss-120b reached for check_referential_integrity and record_finding on its
# first two steps. docs/local-model.md has the traces.
MODEL="${MODEL:-gpt-oss-120b}"
MODEL_GGUF="${MODEL_GGUF:-$HOME/big-local-llms/models/gpt-oss-120b-MXFP4.gguf}"
# The shim that serves a model larger than RAM: --no-repack --load-mode mmap
# --fit off, a 2048 micro-batch and one slot. Typing llama-server by hand
# without those is how a 63GB model gets SIGKILLed instead of paged.
SERVE_SCRIPT="${SERVE_SCRIPT:-$HOME/big-local-llms/scripts/serve-prefetch.sh}"
BASE_URL="${BASE_URL:-http://127.0.0.1:11500/v1}"
DATASET="${DATASET:-testdata/dirty-retail}"
# 12 was not enough twice over: a small model spends its first several steps
# working through list_tables and describe_table, so a short budget stops it
# while it is still orienting and records nothing. Size the budget for the
# model rather than for the dataset — docs/local-model.md, "Budget".
MAX_STEPS="${MAX_STEPS:-24}"
# Ten minutes is the product default and is right for a cloud endpoint. Here
# the *first* step is the expensive one — prefilling a ~6300-token brief against
# weights coming off a disk is nearly half the wall clock of a whole run — and a
# timeout that does not clear it ends the run on provider_error with nothing
# recorded, while the deterministic findings come through untouched.
TIMEOUT="${TIMEOUT:-60m}"
# The effort a reasoning model is asked for, sent by both spellings (see the
# probe below). gpt-oss's harmony template knows low, medium and high and
# quietly defaults anything else: `none` there measured 317 output tokens
# against 132, which presents as a slow model rather than as a setting that did
# not take. For a hybrid Qwen it is the other way round and `none` is the one to
# pass, since openaicompat discards the reasoning on the way back anyway.
EFFORT="${EFFORT:-low}"
# The probe is meant to fail fast, but "fast" is relative to the model. One that
# does not fit in RAM prefills its weights from disk at around a token a second,
# so the 166-token probe alone can take three minutes before it generates
# anything — and five minutes was not enough for gpt-oss-120b on a SATA SSD.
# Failing here on a model that a run would have handled is the expensive
# mistake, since the whole point of the probe is to save the run.
PROBE_TIMEOUT="${PROBE_TIMEOUT:-900}"
# Waiting for a server this script started to answer. mmap makes the load itself
# quick, but the file is 63GB and the first pages come off the disk.
START_TIMEOUT="${START_TIMEOUT:-600}"
# Four of dirty-meters' six agent targets are invisible in the export and become
# visible only when the customer's own dictionary or catalog is read, so a run
# against that fixture with no context server measures the control half and not
# the feature. `auto` wires scripts/context-server to $DATASET/context when the
# dataset has one, which is the same instrument `veritix eval` is scored with;
# `off` is how to run the control deliberately rather than by forgetting.
CONTEXT="${CONTEXT:-auto}"
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

# Veritix speaks one dialect to all of these, but the *diagnostics* are native:
# where the context window is readable, and what "get the model" means, differ
# per server. Identifying which one this is turns the two checks below from
# silence into advice. Both probes are unauthenticated GETs that every install
# answers, and neither loads a model.
native="${BASE_URL%/v1}"
identify() {
	if curl -fsS --max-time 5 "$native/api/tags" >/dev/null 2>&1; then
		echo ollama
	elif curl -fsS --max-time 5 "$native/props" >/dev/null 2>&1; then
		echo llama.cpp
	else
		echo unknown
	fi
}
backend=$(identify)

# ── the server, if there is not one already ────────────────────────────────
#
# Starting it here rather than in another terminal is the whole difference
# between one command and a remembered recipe, and the recipe is three paging
# flags and a micro-batch size that are not guessable. What this must not do is
# take over a server that is already up: that one may be serving something else,
# and it is also how a warm model is kept across runs, since stopping this one
# at exit means the next run reloads.
server_pid=
server_log=
stop_server() {
	[ -n "$server_pid" ] || return 0
	say "Stopping the model server this run started (pid $server_pid)"
	kill "$server_pid" 2>/dev/null || true
	wait "$server_pid" 2>/dev/null || true
	server_pid=
}

if [ "$backend" = unknown ] && ! curl -fsS --max-time 5 "$BASE_URL/models" >/dev/null 2>&1; then
	if [ ! -x "$SERVE_SCRIPT" ] || [ ! -r "$MODEL_GGUF" ]; then
		echo "nothing is listening at $BASE_URL, and this script cannot start one:" >&2
		[ -x "$SERVE_SCRIPT" ] || echo "  no server script at $SERVE_SCRIPT (set SERVE_SCRIPT)" >&2
		[ -r "$MODEL_GGUF" ] || echo "  no model file at $MODEL_GGUF (set MODEL_GGUF)" >&2
		echo "  or point BASE_URL at a server you started yourself" >&2
		exit 1
	fi

	hostport="${native#*://}"
	mkdir -p "$OUT_DIR"
	server_log="$OUT_DIR/$(date +%Y%m%d-%H%M%S)-llama-server.log"
	echo "nothing listening — starting $(basename "$SERVE_SCRIPT")"
	echo "  model: $MODEL_GGUF"
	echo "  log:   $server_log"

	# The alias is MODEL, so the name asked for over the dialect and the name the
	# server answers to are the same string by construction. llama-server's own
	# default alias is the GGUF's full path, which nobody would guess.
	"$SERVE_SCRIPT" -m "$MODEL_GGUF" -a "$MODEL" \
		--host "${hostport%%:*}" --port "${hostport##*:}" \
		>"$server_log" 2>&1 &
	server_pid=$!
	# On a signal, stop and leave rather than falling back into the script: a
	# Ctrl-C in the middle of an audit means the run is over, not that the trace
	# summary below should run against half a file.
	trap stop_server EXIT
	trap 'stop_server; exit 130' INT
	trap 'stop_server; exit 143' TERM

	waited=0
	until curl -fsS --max-time 5 "$native/props" >/dev/null 2>&1; do
		if ! kill -0 "$server_pid" 2>/dev/null; then
			server_pid=
			echo "the server exited while starting; its last lines:" >&2
			tail -20 "$server_log" | sed 's/^/  /' >&2
			exit 1
		fi
		if [ "$waited" -ge "$START_TIMEOUT" ]; then
			echo "the server did not answer within ${START_TIMEOUT}s; see $server_log" >&2
			exit 1
		fi
		sleep 2
		waited=$((waited + 2))
	done
	echo "  ready in ${waited}s"
	backend=$(identify)
fi

echo "server: $backend"

# /models is a convenience, not the gate: the dialect's implementations disagree
# about which endpoints they bother with, and the probe below is what actually
# decides whether a run is worth starting.
if models=$(curl -fsS --max-time 5 "$BASE_URL/models" 2>/dev/null); then
	if ! jq -e --arg m "$MODEL" 'any(.data[]?; .id == $m)' <<<"$models" >/dev/null 2>&1; then
		warn "the server does not list a model called '$MODEL'"
		echo "  it offers: $(jq -r '[.data[]?.id] | join(", ")' <<<"$models")" >&2
		case "$backend" in
		ollama)
			echo "  pull it with: ollama pull $MODEL" >&2
			;;
		llama.cpp)
			echo "  llama-server serves the one model it was started with, so MODEL" >&2
			echo "  has to match its -a/--alias (it defaults to the GGUF's path)" >&2
			;;
		*)
			echo "  set MODEL to one of those, or point BASE_URL at a server with it" >&2
			;;
		esac
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
	jq -n --arg m "$MODEL" --arg e "$EFFORT" '{
    model: $m, max_tokens: 128, temperature: 0,
    # Ask for the same deliberation the run will ask for, by both spellings,
    # because openaicompat sends both and the servers disagree about which one
    # they read. A probe that omits them measures a model reasoning at its
    # default, which on gpt-oss-120b is six times the output of effort=low —
    # so the probe would time out here on a model the run handles fine.
    reasoning_effort: $e,
    chat_template_kwargs: {reasoning_effort: $e},
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
  }
  # An unset effort must ask for nothing rather than ask for "", which is what
  # openaicompat does with the same setting.
  | if $e == "" then del(.reasoning_effort, .chat_template_kwargs) else . end'
)

probe_start=$(date +%s)
probe=$(curl -fsS --max-time "$PROBE_TIMEOUT" "$BASE_URL/chat/completions" \
	-H 'Content-Type: application/json' -d "$probe_body") || {
	{
		echo "The probe failed: $BASE_URL/chat/completions did not answer a two-tool payload."
		echo
		if [ -n "$server_pid" ]; then
			echo "This run started that server, so whatever it has to say is in its log:"
			echo "  $server_log"
			tail -20 "$server_log" | sed 's/^/  /'
		else
			cat <<EOF
Something that was already running is serving $BASE_URL, so it answered an
error — run the same request by hand to see what it said. Two things produce
exactly this failure:

  llama.cpp started without --jinja. That flag is what makes it call tools at
  all, and this passes it along with the flags a model larger than RAM needs:
    $SERVE_SCRIPT -m MODEL.gguf -a $MODEL

  Ollama at its default context window. With no GPU it picks 4096, the first
  agent prompt is ~4080, and the system prompt is then discarded from the front
  mid-run — which reads as a stupid model rather than a truncated one:

    OLLAMA_CONTEXT_LENGTH=32768 OLLAMA_NO_CLOUD=1 OLLAMA_KEEP_ALIVE=60m \\
      OLLAMA_FLASH_ATTENTION=1 ollama serve
EOF
		fi
	} >&2
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

# The context window is not in the OpenAI dialect anywhere, so it has to be read
# natively. Ollama reports it only for a *loaded* model, which is why this runs
# after the probe rather than before it; llama.cpp reports it any time.
#
# A check that cannot run must say so — the same reason `rules` reports a rule
# that never applied. This one used to skip silently on anything that was not
# Ollama, and silence here is indistinguishable from a pass on the setting most
# likely to waste an hour.
ctx=
case "$backend" in
ollama)
	ctx=$(curl -fsS --max-time 5 "$native/api/ps" 2>/dev/null |
		jq -r '.models[0].context_length // empty' 2>/dev/null || true)
	;;
llama.cpp)
	ctx=$(curl -fsS --max-time 5 "$native/props" 2>/dev/null |
		jq -r '.default_generation_settings.n_ctx // empty' 2>/dev/null || true)
	;;
esac
if [ -z "$ctx" ]; then
	warn "could not read this server's context window"
	echo "  The first agent prompt is ~4080 tokens. A server whose window is" >&2
	echo "  smaller discards from the front mid-run, taking the system prompt with" >&2
	echo "  it; the model stops knowing it may not see cell values and answers in" >&2
	echo "  prose, which reads as a stupid model rather than a truncated one." >&2
	echo "  Confirm the window by hand before spending an hour on a run." >&2
elif [ "$ctx" -lt 8192 ]; then
	warn "the loaded context window is $ctx tokens; the first agent prompt is ~4080"
	case "$backend" in
	ollama)
		echo "  restart the server with OLLAMA_CONTEXT_LENGTH=32768 or the system" >&2
		echo "  prompt will be discarded mid-run" >&2
		;;
	*)
		echo "  restart the server with a larger window (llama-server: -c 32768) or" >&2
		echo "  the system prompt will be discarded mid-run" >&2
		;;
	esac
else
	echo "context window: $ctx tokens"
fi

if [ "$mode" = probe ]; then
	exit 0
fi

# ── the run ────────────────────────────────────────────────────────────────

say "Building"
make build >/dev/null

# ── the customer's own documents ───────────────────────────────────────────
#
# Some defects are not in the data. Four of dirty-meters' six agent targets are
# invisible in the export — a status vocabulary, a tariff lifecycle date, what
# register_value *means*, and how site_ref joins to upn — and a run with no
# context server cannot find them however good the model is. That run is worth
# having, but it is the control, and a control taken by accident reads as a
# model that failed.
#
# scripts/context-server is the instrument the eval's aided half is measured
# with, so using it here means the run by hand and the scorecard are asking the
# same question. It is not part of the product: a real deployment connects to
# the dictionary the customer already has.
context_dir=
context_args=()
context_config=

# A server named on the command line is the caller saying what they want, and
# --no-context is the control asked for outright. Either one settles it — the
# same precedence resolveContext applies inside veritix, for the same reason.
for arg in "${extra[@]}"; do
	case "$arg" in
	--context-server | --context-server=* | --no-context)
		CONTEXT=off
		echo "context: left to the flags after --"
		;;
	esac
done

case "$CONTEXT" in
off | none | no | "") ;;
auto)
	if [ -d "$DATASET/context" ]; then context_dir="$DATASET/context"; fi
	;;
*)
	context_dir="$CONTEXT"
	if [ ! -d "$context_dir" ]; then
		echo "CONTEXT=$CONTEXT is not a directory (set CONTEXT=off to run unaided)" >&2
		exit 1
	fi
	;;
esac

if [ -n "$context_dir" ]; then
	context_dir="$(cd "$context_dir" && pwd)"
	context_bin="$repo_root/bin/context-server"
	# --context-server splits its command on whitespace, deliberately: it is a
	# flag for driving a run by hand, and the config file is where a path with a
	# space in it belongs. Say so here rather than letting the parser take the
	# first half of the path as the command.
	case "$context_bin$context_dir" in
	*[[:space:]]*)
		warn "a path here contains whitespace, which --context-server cannot express"
		echo "  write context.servers into a config file instead, or move the fixture" >&2
		exit 1
		;;
	esac

	say "Serving $context_dir over MCP"
	go build -o "$context_bin" ./scripts/context-server
	printf 'documents: '
	for doc in "$context_dir"/*; do
		if [ -f "$doc" ]; then printf '%s ' "$(basename "$doc")"; fi
	done
	printf '\n'
	context_args=(--context-server "docs:$context_bin -dir $context_dir")

	# `serve` takes its context servers from the configuration file alone —
	# they name programs Veritix will start, which is not a decision to make in
	# an environment variable — so the flag the audit uses is not available here
	# and a file has to exist. Writing one is only safe where there is not one
	# already: a generated file that shadowed somebody's own configuration would
	# turn every other setting off silently.
	if [ "$mode" = serve ]; then
		existing="${VERITIX_CONFIG:-}"
		for c in "$repo_root/veritix.yaml" "$repo_root/veritix.yml" \
			"${XDG_CONFIG_HOME:-$HOME/.config}/veritix/config.yaml" \
			"${XDG_CONFIG_HOME:-$HOME/.config}/veritix/config.yml"; do
			if [ -z "$existing" ] && [ -f "$c" ]; then existing="$c"; fi
		done
		if [ -n "$existing" ]; then
			warn "$existing is already this run's configuration, so none was generated"
			echo "  add the documents to it by hand if the UI should offer them:" >&2
			printf '    context:\n      servers:\n        - name: docs\n          command: %s\n          args: ["-dir", "%s"]\n' \
				"$context_bin" "$context_dir" >&2
			context_dir=
		else
			mkdir -p "$OUT_DIR"
			context_config="$OUT_DIR/context-config.yaml"
			cat >"$context_config" <<-EOF
				# Generated by scripts/local-model.sh, because context servers are
				# configured in a file and never in the environment. Overwritten on
				# every --serve run; edit the script rather than this.
				context:
				  servers:
				    - name: docs
				      command: $context_bin
				      args: ["-dir", "$context_dir"]
			EOF
			echo "config:    $context_config"
		fi
	fi
fi

if [ "$mode" = serve ]; then
	say "Serving on http://$ADDR with the agent available"
	echo "The agent is per-run: tick it when starting an audit, or POST /runs with"
	echo '{"agent": true}. The trace lands at /api/v1/runs/<id>/trace.'
	# Not `exec` when this run owns the model server: exec replaces the shell and
	# the EXIT trap with it, and the server would outlive the thing that started
	# it with nothing left to stop it.
	serve=(env)
	[ -n "$server_pid" ] || serve=(exec env)
	if [ -n "$context_config" ]; then
		echo "The documents in $context_dir are offered to the agent; the trace's"
		echo "context section is rendered beside the egress panel."
		serve+=(VERITIX_CONFIG="$context_config")
	fi
	"${serve[@]}" \
		VERITIX_LLM_PROVIDER=openai-compatible \
		VERITIX_LLM_BASE_URL="$BASE_URL" \
		VERITIX_LLM_MODEL="$MODEL" \
		VERITIX_LLM_MAX_STEPS="$MAX_STEPS" \
		VERITIX_LLM_REQUEST_TIMEOUT="$TIMEOUT" \
		VERITIX_LLM_EFFORT="$EFFORT" \
		./bin/veritix serve --addr "$ADDR"
	exit $?
fi

mkdir -p "$OUT_DIR"
stamp="$(date +%Y%m%d-%H%M%S)"
slug="$(printf '%s' "$MODEL" | tr -c 'A-Za-z0-9._-' '-')"
trace="$OUT_DIR/$stamp-$slug.trace.json"
log="$OUT_DIR/$stamp-$slug.log"
report="$OUT_DIR/$stamp-$slug.report.txt"

say "Auditing $DATASET with $MODEL (max $MAX_STEPS steps, ${TIMEOUT}/call, effort=$EFFORT)"
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
	--llm-effort "$EFFORT" \
	--trace-out "$trace" \
	--log-level debug \
	"${context_args[@]}" \
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

# ── what left toward the customer's own servers ────────────────────────────
#
# The trace answers two questions now, and this is the second: what did Veritix
# ask for, and of whom. Every request is a listing or a read of a URI that came
# out of a listing, which is the property that makes "no text the model wrote
# leaves the process" checkable by reading rather than by trusting.
#
# The figure to read is reads against documents offered. A run that listed three
# and read none is a model that did not look — gpt-oss-120b did exactly that on
# three runs of dirty-meters while finding, unaided, the very join one of those
# documents explains — and a recall score cannot tell that apart from a client
# that is broken. This can.
if [ -s "$trace" ] && jq -e '.context != null' "$trace" >/dev/null 2>&1; then
	say "Context"
	jq -r '
    .context as $c
    | "servers:      " + ([$c.servers[] | .name + " (" + (.documents | tostring) + " documents"
        + (if .error != null and .error != "" then ", " + .error else "" end) + ")"] | join(", ")),
      "offered:      \($c.documents | length) — \([$c.documents[].id] | join(", "))",
      "requests:     \([$c.requests[]? | select(.method == "resources/list")] | length) list,"
        + " \([$c.requests[]? | select(.method == "resources/read")] | length) read"
        + " (\([$c.requests[]? | select(.error != null and .error != "")] | length) failed)",
      "admitted:     \($c.documents_read) documents, \($c.bytes_admitted) bytes verbatim"
  ' "$trace"
	if [ "$(jq -r '[.context.requests[]? | select(.method == "resources/read")] | length' "$trace")" -eq 0 ]; then
		warn "the model read none of the documents it was offered"
		echo "  It was connected, the catalog was enumerated and the documents were" >&2
		echo "  named in the brief, so this is a model that did not reach for a tool" >&2
		echo "  rather than a client that failed. On a fixture whose defects live in" >&2
		echo "  those documents it scores the same as the unaided control — which is" >&2
		echo "  what the control is for. docs/local-model.md has the measured run." >&2
	fi
fi

# The egress check from docs/local-model.md, against a real model rather than
# llmtest's scripted one. These are verbatim contents of the fixture; none of
# them may appear in a payload that left the process.
#
# The list is per fixture and has to be, because a check that silently covers
# only the dataset it was written for is indistinguishable from one that
# passed. Prefer values a model could not have invented — names, not codes: a
# model is free to guess `WHERE status = 'delivered'` and be shown its own
# literal back by Guard.EngineError, which is correct and would read here as a
# leak.
raw_values=()
case "$DATASET" in
*dirty-retail)
	raw_values=(
		"CUS-000001" "CUS-000005" "CUS-999999"
		"alice@example.com" "carol@example.com"
		"Alice Smith" "Frank Green"
		"Zürich" "München" "Montréal"
		"Doohickey" "Widget"
		"Quarterly Sales Report"
	)
	;;
*dirty-logistics)
	raw_values=(
		"Aeroline Freight" "Baltic Haulage" "Corvex Logistics"
		"London Bermondsey" "Manchester Trafford" "Dublin Docklands"
		"Madrid Vallecas" "Frankfurt Ost"
	)
	;;
*dirty-meters)
	# This fixture has a second reason to prefer some values over others, and it
	# is the opposite of the first: its context/ documents go to the model
	# *verbatim*, which is M5b's one deliberate exception, so anything they quote
	# would appear in the trace legitimately and read here as a leak. The data
	# dictionary quotes `UPN-4471` while explaining how site_ref joins, names the
	# status vocabulary and the four regions, and the catalog names every tariff
	# code and Economy Seven — so none of those can be on this list. Addresses,
	# postcodes and the remaining tariff names appear in no document, which leaves
	# them exactly what this check wants: contents of the export and nothing else.
	raw_values=(
		"12 Alder Road" "88 Kestrel Way" "3 Marchmont Terrace"
		"207 Foundry Lane" "41 Quarry Rise" "9 Wharf Court"
		"64 Beacon Hill" "15 Priory Gardens"
		"BS1 4TH" "EH9 1HZ" "CF10 5PS" "NE2 1XN"
		"Standard Domestic A" "Standard Domestic B" "Commercial Single Rate"
	)
	;;
esac

if [ -s "$trace" ] && [ "${#raw_values[@]}" -gt 0 ]; then
	say "Egress check"
	leaked=0
	for raw in "${raw_values[@]}"; do
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
elif [ -s "$trace" ]; then
	warn "no egress value list for $DATASET, so nothing was checked"
	echo "  add one to scripts/local-model.sh beside the other fixtures" >&2
fi
