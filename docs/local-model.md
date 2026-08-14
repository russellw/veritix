# Running the agentic auditor against a local model

Veritix's premise is that commercially sensitive data never leaves the
customer's machine. A customer who will not send their ledger to a software
vendor is usually the same customer who will not send it to a model vendor, so
the local-model path is not a nice-to-have alongside Anthropic — for a large
part of the addressable market it is the *only* path, and it has to work.

This document is how to set one up, what it costs, and the two ways it fails
quietly.

It is also the cheapest way to develop against M4. A local model costs nothing
per token, so the loop, the egress guard, the evidence re-execution and the
budget stops can all be exercised as often as you like before spending money on
a frontier model. A *weak* local model is arguably the better test of the
harness: the "model misbehaves → tool error → it corrects itself" path that
`internal/agent` claims to handle is one a small model exercises constantly and
Claude rarely triggers at all.

## Ollama

`openaicompat.DefaultBaseURL` points at `http://localhost:11434/v1`, which is
Ollama, because Ollama is what a customer running a model on their own hardware
most often has. Any other server speaking the chat-completions dialect works
the same way — vLLM, LM Studio, llama.cpp's `llama-server --jinja` — and only
the base URL changes. That last one has been run end to end and measured; see
"llama.cpp, and why Ollama is still the default" below. The others are still
claims.

Installed here the same way Node was: the official tarball with its published
SHA-256 checked, unpacked under `~/.local`, no root and no `curl | sh`.

```sh
V=v0.32.9
cd "$(mktemp -d)"
curl -LO https://github.com/ollama/ollama/releases/download/$V/sha256sum.txt
curl -LO https://github.com/ollama/ollama/releases/download/$V/ollama-linux-amd64.tar.zst
grep 'ollama-linux-amd64.tar.zst' sha256sum.txt | sha256sum -c -   # must print OK
mkdir -p ~/.local/lib/ollama
tar --use-compress-program=unzstd -xf ollama-linux-amd64.tar.zst -C ~/.local/lib/ollama
ln -sf ~/.local/lib/ollama/bin/ollama ~/.local/bin/ollama
```

The tarball is 1.4 GB and unpacks to 2.1 GB, of which about 2.0 GB is
`lib/ollama/cuda_v12`, `cuda_v13` and `vulkan`. On a machine with no GPU that is
dead weight and can be deleted to reclaim it; it is left in place here because
an as-shipped install is easier to reason about than a pruned one.

## Starting it: two settings that are not optional

```sh
OLLAMA_CONTEXT_LENGTH=32768 \
OLLAMA_NO_CLOUD=1 \
OLLAMA_KEEP_ALIVE=60m \
OLLAMA_FLASH_ATTENTION=1 \
ollama serve
```

**`OLLAMA_CONTEXT_LENGTH` is the one that will cost you a day.** Ollama sizes
the context window from available VRAM and settles on **4096 tokens** when there
is no GPU. Veritix's first agent prompt against `testdata/dirty-retail` measures
**~4080 tokens** — the system prompt, the tool schemas, the profile of every
column, and what the deterministic pass already found. That no longer fits at
all, and when it did fit it fit for a step or two: tool results push the
conversation past the window and llama.cpp discards from the front, which is
where the system prompt lives.

So the failure mode is not an error. It is a model that starts well, then
forgets it is not allowed to see cell values, forgets that `record_finding` is
its only output, and starts answering in prose — and every one of those looks
like "the small model is not up to the job" rather than like a misconfigured
context window. Set it explicitly and it does not happen.

**`OLLAMA_NO_CLOUD=1`** disables Ollama's remote-inference and web-search
features. For a product whose entire proposition is that data does not leave the
customer's machine, a model runtime that can transparently forward a request to
somebody else's GPU is not something to leave to a default. This is belt and
braces — Veritix's own egress guard bounds what is in the payload either way —
but the whole design is layered on the assumption that any single layer might be
wrong.

`OLLAMA_KEEP_ALIVE=60m` keeps the weights resident between runs; the default 5m
means paying the model load again every time you go and read something.

## llama.cpp, and why Ollama is still the default

Ollama is regularly described as adding nothing but overhead on top of
llama.cpp. That is worth answering with a measurement rather than an opinion,
because if it were true the default here would be wrong.

It is half true. Ollama has grown its own engine for newer architectures, so in
general it is a second implementation with its own bugs rather than a thin shim
— but for the models used here it is still a wrapper, literally. Serving
`qwen3:4b-instruct-2507-q4_K_M`, `pgrep` shows Ollama's runner is
`lib/ollama/llama-server`, the same binary this section installs, invoked with
`-c 32768 --flash-attn on --context-shift`.

That is what makes the overhead claim testable, and it does not survive contact
with this workload: both run the same GGML kernels over the same GGUF, and on
the hardware below they generate at the same speed. What Ollama actually costs
is **defaults**, not throughput, and the one that matters is the context window
already documented above. What it buys is a model registry, automatic memory
fit, hot-swapping between models, and a one-file install on Windows — which is
not nothing for a product whose users are on Windows desktops.

So Ollama stays the default because it is what a customer most often has, and
that makes it the environment worth developing against. llama.cpp's server is
supported and measured, not merely claimed to work.

The CPU build is a 16.6 MB tarball unpacking to 41 MB, against Ollama's 1.4 GB
and 2.1 GB — most of that difference being the CUDA and Vulkan libraries noted
above. Installed the same way as everything else here: the official tarball with
its published SHA-256 checked, unpacked under `~/.local`, no root.

```sh
V=b10423
cd "$(mktemp -d)"
curl -fsSL -O https://github.com/ggml-org/llama.cpp/releases/download/$V/llama-$V-bin-ubuntu-x64.tar.gz
# the release API publishes each asset's digest; check it before unpacking
curl -fsS https://api.github.com/repos/ggml-org/llama.cpp/releases/tags/$V |
    jq -r '.assets[] | select(.name == "llama-'$V'-bin-ubuntu-x64.tar.gz") | "\(.digest[7:])  \(.name)"' |
    sha256sum -c -                                  # must print OK
mkdir -p ~/.local/lib/llama.cpp
tar -xzf llama-$V-bin-ubuntu-x64.tar.gz -C ~/.local/lib/llama.cpp
```

**`--jinja` is what makes it call tools at all.** Without it the server ignores
the `tools` array and answers in prose, which is the exact failure the probe
exists to catch, so it fails there rather than twenty minutes into a run.

```sh
LD_LIBRARY_PATH=~/.local/lib/llama.cpp/llama-$V \
~/.local/lib/llama.cpp/llama-$V/llama-server \
    -m ~/.ollama/models/blobs/sha256-<the model layer> \
    --jinja -c 32768 -t 4 --host 127.0.0.1 --port 11436 \
    -a qwen3:4b-instruct-2507-q4_K_M
```

Note the model path: **Ollama's blobs are plain GGUF files**, so llama-server
reads them directly and the two servers share one model store — no conversion,
and no second copy of an 18 GB download. Find the layer with

```sh
jq -r '.layers[] | select(.mediaType | endswith(".model")) | .digest | sub(":"; "-")' \
    ~/.ollama/models/manifests/registry.ollama.ai/library/qwen3/4b-instruct-2507-q4_K_M
```

The `sub(":"; "-")` is not decoration: the manifest writes the digest
`sha256:85e4…` and the file on disk is `sha256-85e4…`, so pasting the digest
verbatim gets you a file-not-found.

`-a` sets the id the server reports, so it is what `MODEL` has to match;
llama-server serves one model per process, where Ollama loads on demand.

Then the script drives it like anything else:

```sh
BASE_URL=http://127.0.0.1:11436/v1 scripts/local-model.sh
```

### Measured, same machine, same model, same fixture

`llama-server b10423` against `qwen3:4b-instruct-2507-q4_K_M`, 24 steps, beside
the Ollama run that also ran 24 steps:

| | llama.cpp | Ollama |
|---|---|---|
| Median step | 94 s | 94 s |
| First step (cold prefill) | 612 s | 287 s (688 and 689 s on other runs) |
| End to end | 49m 0s | 39m 47s |
| Tool calls | 24, 0 refused | 24, 0 refused |
| Malformed calls needing correction | 0 | 0 |
| Findings recorded | 0 | 0 |

**The median step is identical**, which is the number that answers the
efficiency question: there is no throughput difference for this model on this
hardware. The end-to-end gap is the first step, and first-step prefill is noisy
on both — Ollama's ranged from 287 s to 689 s across runs depending on how warm
it was. The deterministic report body came back byte-identical, and the egress
check passed with 34 values shaped.

Recording no findings is not a llama.cpp result: across seven Ollama runs of
this same model the counts were 0, 0, 0, 3, 0, 3, 2. This run's tool mix is
nearly a twin of the Ollama 24-step run that also recorded nothing and also died
on the step budget. That is the nondeterminism this document keeps warning
about, which is why the comparison above is about the plumbing and not about
audit quality.

What this establishes is that `openaicompat` is not accidentally shaped to
Ollama's quirks. What it does not establish is vLLM or LM Studio, or the error
paths — this model behaved, so nothing exercised a refused query or a corrected
count.

## Choosing a model

The requirement is tool calling that works through Ollama's OpenAI-compatible
shim, in as few parameters as possible. Two things matter more than benchmark
scores:

**Take the non-thinking variant, or turn thinking off.** Qwen3's hybrid models
emit a reasoning block before every tool call. On a CPU those are tokens
generated at the same speed as useful ones, several hundred of them per step,
and `openaicompat` drops them on the way back anyway because this dialect has
nowhere to replay them. `2507` instruct tags are the non-thinking refresh:

```sh
ollama pull qwen3:4b-instruct-2507-q4_K_M          # 2.5 GB, the small one
ollama pull qwen3:30b-a3b-instruct-2507-q4_K_M     # 18 GB, 3B active
```

Newer families have no such tag — every `qwen3.5` variant is hybrid — and there
the switch is `--llm-effort none`, which `openaicompat` sends as
`reasoning_effort` and Ollama honors. Measured on `qwen3.5:35b-a3b-q4_K_M`, one
tool call:

| | completion tokens |
|---|---|
| default | 73 |
| `--llm-effort none` | **14** |

Five times the generation, on hardware where generation is the entire cost, for
reasoning that is discarded on the way back. `scripts/local-model.sh` therefore
passes `none` by default; the non-thinking instruct models accept it and ignore
it. Ollama's native API spells the same thing `"think": false`, which is no use
here because Veritix speaks the OpenAI dialect — but `reasoning_effort` reaches
the same switch, and `chat_template_kwargs` and a bare `think` field do not.

**Prefer a mixture-of-experts model if the RAM is there.** On a CPU the cost per
token follows the *active* parameters, so the 30B-A3B above runs at about the
4B's speed with eight times the total capacity, while a dense 14B would be
several times slower than either. See the measurements below.

**Check it emits `tool_calls` before running a full audit.** One request against
`/v1/chat/completions` with a two-tool payload settles in twenty seconds what a
full audit takes twenty minutes to discover. A server that ignores `tool_choice`
and answers in prose is a documented limitation of this dialect, not a bug in
the loop, and the loop will politely record nothing for as long as you let it.

## What it costs, measured

On the development machine here — an i5-7300U, two physical cores, no GPU,
30 GB RAM — against `testdata/dirty-retail` with `qwen3:4b-instruct-2507-q4_K_M`:

| | |
|---|---|
| Deterministic pass (36 findings) | ~3 s |
| First agent prompt | ~4080 tokens (3540 before the profile moved into it) |
| Prefill | 12–17 tokens/s |
| Generation, short context | 5.6 tokens/s |
| Generation, ~3.5k context | 1.7 tokens/s |
| First step (cold prefill) | 4m 50s |
| Each later step | 30–100 s |
| 12-step run, end to end | 18m 32s, 72,575 tokens |

Generation slows by three times between a 225-token context and a 3.5k one:
this is memory bandwidth, and it is the ceiling. Prefill is only paid once per
prefix, because llama.cpp caches the KV prefix and each step re-prefills only
the new tool result — which is why the first step costs five minutes and the
rest cost one.

The honest summary is that this hardware is good for *validating the plumbing*
and useless for *measuring audit quality*. A run is a coffee break, not an
iteration loop. Anything about whether the agent finds good problems needs
either a frontier model or a machine with a GPU.

## Running it

`scripts/local-model.sh` is everything below, in order, so that a run is one
command rather than six remembered flags:

```sh
scripts/local-model.sh            # probe, audit, summarize the trace, check egress
scripts/local-model.sh --probe    # just the probe: is this server usable at all
scripts/local-model.sh --serve    # the web interface, wired to the local model
scripts/local-model.sh -- --rules my.yaml --format json   # extra veritix flags
```

`MODEL`, `BASE_URL`, `DATASET`, `MAX_STEPS`, `TIMEOUT`, `OUT_DIR` and `ADDR`
override the defaults, which are the ones this document arrived at. Traces, logs and reports
land in `local-runs/`, timestamped and named after the model, because the
interesting comparison is against the previous run rather than against nothing.
The rest of this section is what the script does and why each part is there.

From the CLI:

```sh
VERITIX_LLM_REQUEST_TIMEOUT=30m ./bin/veritix audit testdata/dirty-retail \
    --llm openai-compatible \
    --llm-base-url http://localhost:11434/v1 \
    --llm-model qwen3:4b-instruct-2507-q4_K_M \
    --llm-max-steps 24 \
    --trace-out trace.json \
    --log-level debug
```

`--trace-out` writes the same document the API serves at `/runs/{id}/trace`:
every payload in both directions, verbatim, which is what you want when the
question is why a model did something odd. It is refused up front if no model is
configured, and refused alongside `--output -` rather than interleaving two
documents on stdout. `--log-level debug` is still worth adding while a run is in
progress, since `msg="tool call"` lines arrive as they happen where the trace
only appears at the end.

Checking the egress promise is then a line, against a real model rather than
against `llmtest`'s scripted one:

```sh
grep -c 'CUS-000001' trace.json          # want 0
```

Over HTTP the same trace is stored per run, which is the better way to watch a
long one:

```sh
VERITIX_LLM_PROVIDER=openai-compatible \
VERITIX_LLM_BASE_URL=http://localhost:11434/v1 \
VERITIX_LLM_MODEL=qwen3:4b-instruct-2507-q4_K_M \
VERITIX_LLM_MAX_STEPS=30 \
./bin/veritix serve
```

then `POST /runs` with `"agent": true` and read
`/api/v1/runs/$ID/trace` when it finishes.

**This is deliberately a script and not a `make` target.** `make e2e` drives
`e2e/stub-model.mjs`, which is scripted, deterministic and takes seconds; a real
local model is twenty minutes and gives a different answer every time. Those are
different activities, and a test suite that depends on the second would be a
test suite nobody runs. The scripted model stays the one in CI. Convenience is
worth having anyway, which is what `scripts/local-model.sh` is for — the point
of keeping it out of `make` is that nothing runs it by accident, not that it
should be tedious.

## Budget: small models do not ration themselves

The first 12-step run spent **six consecutive steps on `describe_table`**,
enumerating every table in the dataset before investigating anything, then ran
out of budget with nothing recorded. That is not a malfunction — the system
prompt does say to start with `list_tables` and `describe_table` — but a
frontier model reads five tables' worth of profile and forms a hypothesis, while
a 4B model works through the list.

So the step budget has to be sized for the model, not for the dataset. Below
about 20 steps a small model will still be orienting when the budget stops it,
and the report will say so:

```
investigated by qwen3:4b-instruct-2507-q4_K_M (openai-compatible): 12 steps, 12 tool calls, 0 findings
no cell values were sent to the model; 17 were replaced by their shape
! the investigation stopped early (step_budget), so it may be incomplete
```

Which is the right behavior and worth keeping: a truncated investigation that
says it was truncated is honest, where a truncated investigation reported as a
clean bill of health would be the worst thing this product could do.

Two further 12-step runs ended the same way — `step_budget`, nothing recorded —
which is enough evidence that the default was wrong rather than that the runs
were unlucky. `scripts/local-model.sh` now defaults to **24 steps**, above the
point where orientation stops eating the whole budget, and to a **30-minute**
per-call timeout with it: a longer run reaches the slow, full-context steps that
outrun the product default of ten minutes, and there is no sense in buying more
budget only to end on `provider_error` before spending it. Both are still
`MAX_STEPS` and `TIMEOUT` in the environment. On hardware like the machine
measured above, expect a 24-step run to take the better part of an hour; on
anything with a GPU it is the cheaper half of the trade.

## What a second, longer run found

A 30-step run over HTTP got 17 steps in 56 minutes and then died on
`stopped=provider_error`. Three things came out of it, and all three are about
Veritix rather than about Ollama.

**Per-step cost grows with context, badly.** The KV prefix cache works — each
step re-prefills only the new tool result, 37 to 750 tokens — but throughput
collapses as the window fills. At 225 tokens of context the model generates at
5.6 tokens/s; at 8.2k it generates at **0.78** and prefills at 1.0. Steps that
cost 30 seconds early cost two and a half minutes by step 15. The practical
ceiling on this hardware is about 15 steps *per hour*, and more budget does not
buy proportionally more investigation — it is a reason to expect a 24-step run
to be long, not a reason to keep the budget below what the model needs to reach
a hypothesis.

**A self-imposed timeout is being retried as if it were a network failure.**
`openaicompat` marks every transport error `Retryable` (`openaicompat.go`,
`p.http.Do`), and that includes `llm.request_timeout` expiring. `complete()`
guards on the *parent* context, but the deadline lives on a child, so the parent
is still healthy and the loop re-sends the identical request — three times, each
waiting the full ten minutes. Half of that 56-minute run was the same question
asked three times of a model that was simply slow, and the run then ended in an
error rather than stopping cleanly.

The retry half is fixed: `complete()` now distinguishes its own expired deadline
from a transport failure and returns instead of re-sending, with an error naming
the setting to change. `TestAnExpiredDeadlineIsNotRetried` pins it, and
`TestATransientFailureIsRetried` pins the other half, because the cheap fix
— stop retrying — would have ended an audit on one dropped connection.

The setting itself is still yours to raise: ten minutes is a sane default for a
cloud endpoint and too short for a local model once its context is full, so use
`VERITIX_LLM_REQUEST_TIMEOUT=30m` here. The default is left alone deliberately,
since making every cloud user wait thirty minutes on a hung request would be a
worse trade than the one line of configuration.

**The model mistook shapes for values.** By step 16 it was writing

```sql
WHERE "column0" LIKE 'XXX-9%' AND "column1" LIKE 'XXXXXX%'
WHERE "city" LIKE 'X%'
```

having read `XXX-999999` in the profile and taken it for the contents of the
column. An earlier step invented `'99.99', '999.99', '-99.99'` the same way. All
of them correctly returned zero.

This is the one that matters for the design. The system prompt explains shapes in
as many words, and a 4B model still loses the distinction once its context is
full of them. The consequence is not a leak — the guard is unaffected, and the
run was honest about finding nothing — but it is wasted budget and it would be
wasted budget on a frontier model too, just less often. Worth considering: render
shapes in a form that cannot be mistaken for content (`⟨XXX-999999⟩`), or have
`run_sql` notice a literal that looks like a shape and say so in the tool result.
The second is better, because it corrects the model at the point of the mistake,
which is the same mechanism `record_finding` already uses for an inflated count.

**Against `testdata/dirty-retail`, this model recorded nothing in either 12-step
run.**
That is a fair result rather than a broken one — the deterministic pass hands it
36 findings and tells it not to re-report them, which is a high bar for 4B — but
it does mean a local model of this size validates the machinery without saying
anything about whether the agent adds value. That question needs a bigger model.

## What the 24-step run found, and the change it produced

The raised budget was spent as intended: six steps on `describe_table`, three on
`check_candidate_key`, three on `check_referential_integrity`, then six `run_sql`
counts and five `sample_values` calls — ground the 12-step runs never reached.
39m47s, one tool call per step, no prose, no tool errors, and again nothing
recorded.

**The step budget was not the constraint, and the trace says why.** At step 13
`check_referential_integrity(sales.xlsx#Q1.region → regions.csv.region_code)`
returned `orphans: 2` of six references, with the evidence query attached. That
relationship is not in the deterministic report: `relate.go` pairs columns by
naming convention and overlap, and `region` against `region_code` is exactly the
pair it does not propose. So the agent had, with eleven steps in hand, the one
thing it exists to produce — a real defect the deterministic pass misses — and it
spent those eleven steps on counts that returned nothing interesting.

The brief lists the known findings and the system prompt says to record what it
can prove. Both had been true for twelve steps by then. What was missing was
anything at the point where the evidence arrived, so that is where it went:
`check_referential_integrity` and `check_candidate_key` now add a `note` to their
own result saying either which deterministic rule already covers this defect, or
that none does and to record it now with `evidence_query` as the `count_query`.
`TestACheckSaysWhetherTheDefectIsAlreadyKnown` pins both halves against these two
relationships. It is the same shape as the count correction in `record_finding`:
tell the model where it is looking, not in a prompt it read twenty minutes ago.

The other half of that note matters as much. Steps 11 and 12 re-measured the two
orphan relationships the deterministic pass *does* report, which the brief had
already listed; a tool that says "already reported, do not re-report" spends one
step to save several.

**`sample_values` on a shaped column is close to information-free**, and four of
the last five steps went on it. Step 20 returned three distinct entries whose
shapes all render `XXXX`, so the model learned three counts and nothing else.
This is the same shape/value confusion as above seen from the other side, and it
is unfixed: a column of fixed-width codes cannot be told apart by shape, so
either the tool should say that the shapes collapsed or it should decline to
answer for such a column.

## A bigger local model, and what it settled

The lever on a CPU is *active* parameters, not total ones: generation is
memory-bandwidth-bound, so a dense 14B costs about 3.5× what a 4B costs per
token, while a mixture-of-experts model reads only the experts it activates.
`qwen3:30b-a3b-instruct-2507-q4_K_M` is 30B total and 3B active — eight times
the capacity of the 4B at roughly its per-token cost. Same family, same 2507
instruct line, so capacity is the only variable that changes.

It fits this machine and not much more: 18GB of weights, 22GB resident with the
32k context, ~7GB spare, no swap. `qwen3.5:35b-a3b-q4_K_M` is the same class a
generation newer and worth trying next; `qwen3.5:122b-a10b` would not fit.

| | 4B | 30B-A3B |
|---|---|---|
| wall clock | 65m / 23 steps | 48m / 24 steps |
| median step | 101s | 115s |
| tokens | 177k in, 2.0k out | 178k in, 1.0k out |
| outcome | `finished`, 3 findings | `step_budget`, 0 findings |

The cost prediction held exactly. The audit result was worse — and the trace
says why, in a way that was worth more than the finding would have been.

**Both models read shapes as values, and the bigger one did it more.** Five of
the 30B's seven `run_sql` calls were variations on `WHERE "region" = 'XXXX'`,
with `'99.99'` and `'9,999.99'` alongside; every one correctly matched nothing.
The 4B had done the same thing more quietly, in an inert `NOT IN` clause, and
its two junk findings came from the same confusion. Two models eight times apart
in capacity making the identical mistake is not a small model being silly: a
bare shape sits in a tool result exactly where a value would sit and looks like
one, and no amount of system prompt survives twenty steps of a filling context.

So shapes are now delimited — `⟨XXX-999999⟩` — everywhere they reach a model.
That is a change to the representation rather than a rule against one mistake,
which matters: a detector for shape-shaped literals would have caught these two
runs and nothing else, and the next model would find a new way to be confused by
an ambiguous encoding. The brackets travel with the shape. They also make
padding visible for free (`⟨  XXX XXXXX  ⟩`), which a bare shape hid.

**It was one step from the finding.** Steps 14 and 17 were `sample_values` calls
on `regions_csv.region` and `sales_xlsx_q1.region_code` — the right relationship
with the table and column crossed, refused both times — and step 24, its last,
finally wrote the join by hand and returned 1. It never ran
`check_referential_integrity` on that pair, so it never got the note that would
have told it this was new ground.

**The orientation tax is the same for both.** Eight of 24 steps went on
`list_tables` and `describe_table` for seven tables, before either model did any
work — a third of the budget spent acquiring data the deterministic pass already
holds. So the brief carries the profile now: every column's declared type
against its actual type, missing counts, distinct counts, shapes and
distributions, rendered by the same code `describe_table` uses so the two cannot
disagree. It measures **+540 tokens on the first prompt** against eight steps
saved, each of which cost a full model call and 30–150 seconds here. A dataset
too wide to fit is described as far as the budget goes and names the rest in
`described_on_request`.

## What the two fixes bought, and the third failure underneath

The 4B again, with shapes delimited and the profile in the brief:

| | orientation calls | steps | findings |
|---|---|---|---|
| 4B, before either fix | 7 | 24/24 `step_budget` | 0 |
| 4B, + the check-tool note | 8 | 23/24 `finished` | 3 (1 real, 2 junk) |
| 30B-A3B, + the note | 8 | 24/24 `step_budget` | 0 |
| 4B, + delimiters + profile in brief | **1** | **13/24 `finished`** | 0 |

**Orientation went from eight steps to one.** It called `list_tables` once out
of habit and was running `check_referential_integrity` by step 2, reaching at
step 4 the region-code finding that took the previous 4B run eleven steps and
the 30B all twenty-four. The whole investigation was done by step 12, with
eleven steps of budget unspent.

**No shape reached SQL.** All three `run_sql` calls were clean referential
counts, against five of seven for the 30B before the delimiters. Three queries
is encouraging rather than conclusive, and the confusion has not gone: writing
prose at the end, the model *stripped the brackets* and reasoned about `'99.99'`
and `'#XXX!'` as though they were contents. The representation fix stops shapes
reaching the engine; it does not make a 4B model understand what a shape is.

**Then it wrote its tool calls out as text.** Step 13, `end_turn`, fourteen
minutes of generation containing three complete `record_finding` payloads —
the first of them the real finding, with a count query that would have
reproduced at 2. The loop saw a model that had stopped calling tools, called
that a clean finish, and recorded nothing.

That is a malformed tool call, and Veritix has one mechanism for those: hand it
back and let the model correct itself. `writtenCall` now matches a JSON object
in a final message against the tool schemas and the loop says so, once. It does
*not* execute what the model wrote — that would put a finding in the report
without going through the tool that checks the count against the query, which
is the whole basis of the design.

## The 35B run, and the mirage it walked into

`qwen3.5:35b-a3b-q4_K_M` with `--llm-effort none`: 13 steps, 55 minutes, **7 of
12 tool calls refused**, nothing recorded. The trace says why, and it was
Veritix's fault rather than the model's.

`Guard.EngineError` shapes every single-quoted run in a DuckDB error, so that a
cell value quoted in a diagnostic cannot escape. DuckDB also echoes the
offending statement back inside the message. Once shapes acquired delimiters,
the model's own literals started coming back visibly rewritten:

```
sent:     REPLACE(amount, ',', '')      WHERE substring(amount,1,1) = '-'
returned: REPLACE(amount, '⟨,⟩', '⟨⟩')  WHERE substring(amount,1,1) = '⟨-⟩'
```

It drew the obvious conclusion — *"the SQL engine is consistently
misinterpreting literal strings in WHERE clauses"*, its own words at step 13 —
and stopped writing SQL. Before the delimiters this was invisible, because
punctuation is a fixed point of the shape function and `'-'` shaped to `'-'`.
The change did not create the flaw; it made it legible to the model.

The fix is the converse of the egress rule: text the model sent cannot be
disclosed by sending it back. `EngineError` now takes the statement and passes
through any quoted literal that appears in it verbatim. A conversion error
quoting `'CUS-004417'` is still shaped, because that never appeared in the
model's query. Function signatures in binder errors — `'length(BIGINT)'` — are
still shaped, which is a safe false positive and still costs the model a
diagnostic; the *Candidate functions* list underneath survives intact.

The lesson worth keeping is about testing. Every scripted test passed
throughout, because `llmtest` never writes bad SQL. This class of defect —
Veritix corrupting its own conversation with the model — only appears when
something on the other end is trying to do a job.

### Two more 35B runs, and the thing that does not fix

With honest errors the same model ran twice more. It recorded nothing either
time, and the traces say why:

| | tool calls | outcome |
|---|---|---|
| errors rewritten | 12 (7 refused) | fought the mirage, 55m |
| errors honest | 24 (6 refused) | `step_budget`, 29m |
| prompt naming the check tools | 4 (1 refused) | stopped itself at step 5, 21m |

**Forty tool calls across three runs, every one of them `run_sql`.** Not one
call to `check_referential_integrity`, `check_candidate_key`, `sample_values`,
`profile_column` or `record_finding`. It hand-wrote the orphan check that a
tool answers in one call, then spent a step asking which customer and got a
shape back; it spent six consecutive steps on `SELECT DISTINCT region_code`
whose every answer was an indistinguishable `⟨XXXX⟩`. The third run added a
sentence to the system prompt naming the check tools and saying what they save.
It changed nothing, so that sentence was reverted: a prompt line that does not
do the thing it was added for is worse than a shorter prompt.

The run then ended at step 5 of 24 with seven numbered "Discrepancies" written
out in prose, mid-sentence, having measured none of them. `writtenCall` did not
fire and should not have — it matches a JSON object against the tool schemas,
and this was English. That mechanism catches a fumbled handover, not a model
that never intended to hand anything over.

**The smaller model does this job and the larger one does not.**
`qwen3:4b-instruct-2507` uses the check tools, records the region-code finding
the deterministic pass misses, and gets its count right; `qwen3.5:35b-a3b` is
eight times the size and has recorded nothing in three attempts. That is not
capability in any simple sense — it is a prior. One model follows the tool
descriptions it is given; the other has decided that auditing means writing SQL
and narrating the results, and no amount of prompt says otherwise.

So the practical advice for choosing a local model is not "take the biggest one
that fits". It is: **probe whether it will use a tool it was not asked to use.**
Two audits against a small fixture cost an hour and settle it; parameter count
does not predict it at all.

## What this is good for, and what it is not

Good for: the loop, the tool surface, the egress guard, evidence re-execution,
the budget stops, the trace, and the wire format of a provider. All of those were
exercised here for free, and one real defect (the retry above) fell out of the
first hour.

Not good for: whether the agent finds *good* problems. Nothing about a 4B model
on a 2017 laptop CPU generalizes to that, and pretending otherwise would be the
same mistake as trusting a model's own count.

The egress check, though, is the one that transfers completely — it is a property
of Veritix, not of the model:

```
redaction: {"shaped": 14, "masked": 0, "passed": 5, "truncated": 0, "sealed": 17}
raw fixture values found in the entire trace: none
```
