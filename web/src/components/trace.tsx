import { useEffect, useState } from 'react'

import * as api from '../api'
import type { AgentTrace, ContextTrace, TraceCall } from '../api'
import { count, duration } from '../format'

/*
The trace view exists to answer one question a customer is entitled to ask
after being told a model looked at their data: what exactly did you send it?

So it shows every payload verbatim rather than a tidy summary of them. A
summary of what left the process is not evidence about what left the process,
and this screen is the difference between the egress promise being checkable
and being merely asserted.
*/

export function Trace({ runId }: { runId: string }) {
  const [trace, setTrace] = useState<AgentTrace | null>(null)
  const [error, setError] = useState('')

  useEffect(() => {
    const ac = new AbortController()
    setTrace(null)
    setError('')

    api
      .getTrace(runId, ac.signal)
      .then((t) => {
        if (!ac.signal.aborted) setTrace(t)
      })
      .catch((e: unknown) => {
        if (ac.signal.aborted) return
        setError(e instanceof Error ? e.message : String(e))
      })

    return () => ac.abort()
  }, [runId])

  if (error) return <p className="notice error">{error}</p>
  if (!trace) return <p className="empty">Loading…</p>

  const withheld = trace.redaction.shaped + trace.redaction.masked

  return (
    <section className="trace">
      <p className="sub">
        {trace.model} via {trace.provider} · {trace.steps.length} steps ·{' '}
        {count(trace.usage.input_tokens)} in / {count(trace.usage.output_tokens)} out ·{' '}
        {duration(trace.duration_ms)}
      </p>

      {/*
        The egress policy is the first thing on this screen, not a footnote:
        it is what the reader came to check.
      */}
      {trace.values_allowed ? (
        <p className="notice warn">
          Cell values were permitted for this run. Values sent to the model had
          obvious identifiers masked and were truncated, but they were your
          data.
        </p>
      ) : (
        <p className="notice">
          No cell value was sent to the model. What it was given is measurements
          — counts, ratios, and shapes with digits as 9 and letters as X —
          across {count(trace.redaction.sealed)} tool results totalling{' '}
          {count(trace.redaction.bytes)} bytes, of which {count(withheld)} value
          {withheld === 1 ? ' was' : 's were'} reduced to a shape on the way out.
          Everything below is shown exactly as it was sent.
        </p>
      )}

      {trace.context && <Context context={trace.context} />}

      {trace.not_reproduced > 0 && (
        <p className="notice">
          {count(trace.not_reproduced)} proposed finding
          {trace.not_reproduced === 1 ? '' : 's'} did not reproduce when the
          engine ran the evidence, and {trace.not_reproduced === 1 ? 'was' : 'were'}{' '}
          discarded rather than reported.
        </p>
      )}

      {!isComplete(trace) && (
        <p className="notice error">
          {stoppedReason(trace)} The findings it did record are still evidence-backed,
          but the investigation may be incomplete.
        </p>
      )}

      <ol className="steps">
        {trace.steps.map((step) => (
          <li key={step.step}>
            <div className="step-head">
              <span className="step-n">Step {step.step}</span>
              <span className="sub">
                {count(step.usage.input_tokens)} in / {count(step.usage.output_tokens)} out ·{' '}
                {duration(step.duration_ms)}
              </span>
            </div>

            {step.thinking && <blockquote className="thinking">{step.thinking}</blockquote>}
            {step.text && <p className="said">{step.text}</p>}

            {step.calls?.map((call, i) => <Call key={i} call={call} />)}
          </li>
        ))}
      </ol>
    </section>
  )
}

function Call({ call }: { call: TraceCall }) {
  const [open, setOpen] = useState(false)

  return (
    <div className={`call${call.is_error ? ' failed' : ''}`}>
      <button className="call-head" onClick={() => setOpen(!open)} aria-expanded={open}>
        <code>{call.tool}</code>
        <span className="sub">
          {call.is_error ? 'refused' : 'answered'}
          {call.duration_ms > 0 && <> · {duration(call.duration_ms)}</>}
        </span>
      </button>

      {open && (
        <>
          <div className="evidence-label">What the model asked for</div>
          <pre>{format(call.arguments)}</pre>
          <div className="evidence-label">What it was sent back, byte for byte</div>
          <pre>{format(call.result)}</pre>
        </>
      )}
    </div>
  )
}

/*
Both sides of a call are shown as formatted JSON where they parse as JSON and
verbatim where they do not. Nothing is elided: a trace that hid a long result
would be exactly as useless as no trace at all.
*/
function format(value: unknown): string {
  if (value === undefined || value === null) return '(nothing)'
  if (typeof value === 'string') {
    try {
      return JSON.stringify(JSON.parse(value), null, 2)
    } catch {
      return value
    }
  }
  return JSON.stringify(value, null, 2)
}

function isComplete(trace: AgentTrace): boolean {
  return trace.stopped === 'finished'
}

function stoppedReason(trace: AgentTrace): string {
  switch (trace.stopped) {
    case 'step_budget':
      return `The investigation stopped at its limit of ${trace.max_steps} steps.`
    case 'token_budget':
      return `The investigation stopped at its token budget of ${count(trace.token_budget ?? 0)}.`
    case 'provider_error':
      return `The model could not be reached: ${trace.error ?? 'no reason given'}.`
    case 'refused':
      return 'The model declined the request.'
    case 'canceled':
      return 'The audit was stopped while the investigation was running.'
    default:
      return 'The investigation ended early.'
  }
}

/*
The context panel is the outbound half of the same promise.

Everything else on this screen answers "what was the model sent". Once Veritix
can fetch the customer's own documents it has to answer a second question — what
did Veritix send, and to whom — because a context server is the first thing
since the model that anything leaves the process toward. So every request is
listed rather than counted: the point a reader is checking is that each one is
a listing, or a read of a URI that came out of a listing, and never a string the
model wrote.
*/
function Context({ context }: { context: ContextTrace }) {
  const failed = context.servers.filter((s) => s.error)

  return (
    <section className="context-trace">
      <p className="notice">
        This run could read {count(context.documents?.length ?? 0)} of your own
        documents, from{' '}
        {context.servers.map((s) => s.name).join(', ')}. The model read{' '}
        {count(context.documents_read)} of them, totalling{' '}
        {count(context.bytes_admitted)} bytes, and those went to it as you wrote
        them — a data dictionary reduced to shapes would explain nothing.
        Veritix chose what to request: the model names a document by its id, and
        the id is looked up here, so nothing it wrote was sent on.
      </p>

      {failed.map((s) => (
        <p className="notice warn" key={s.name}>
          {s.name} could not be reached ({s.error}), so this audit ran without
          it.
        </p>
      ))}

      {context.documents && context.documents.length > 0 && (
        <ul className="context-docs">
          {context.documents.map((d) => (
            <li key={d.id}>
              <code>{d.id}</code> <span className="sub">{d.server}</span>
              {d.description && <span className="sub"> · {d.description}</span>}
            </li>
          ))}
        </ul>
      )}

      {context.requests && context.requests.length > 0 && (
        <details className="context-requests">
          <summary>
            {count(context.requests.length)} request
            {context.requests.length === 1 ? '' : 's'} Veritix made
          </summary>
          <ol>
            {context.requests.map((r, i) => (
              <li key={i}>
                <code>{r.method}</code> <span className="sub">{r.server}</span>
                {r.uri && <code className="uri">{r.uri}</code>}
                <span className="sub">
                  {' '}
                  · {duration(r.duration_ms)}
                  {r.bytes ? ` · ${count(r.bytes)} bytes` : ''}
                </span>
                {r.error && <span className="notice error">{r.error}</span>}
              </li>
            ))}
          </ol>
        </details>
      )}
    </section>
  )
}
