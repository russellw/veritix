import { useEffect, useState } from 'react'

import * as api from '../api'
import type { AgentTrace, TraceCall } from '../api'
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
    case 'cancelled':
      return 'The audit was stopped while the investigation was running.'
    default:
      return 'The investigation ended early.'
  }
}
