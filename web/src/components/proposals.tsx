import { useEffect, useState } from 'react'

import * as api from '../api'
import type { ProposalDetail, ProposalInfo, Severity } from '../api'

/*
The accept screen: where a proposed rule stops being a suggestion.

This is the half of rule proposal that the business user was always going to
need, because the alternative asks somebody on a Windows desktop to merge YAML
by hand. A defect the model found on one run is found on every run once the rule
that catches it is in force — and from then on it costs no model, no tokens and
no waiting.

Two things are load-bearing here and neither is decoration. The first is that
the values are shown: a one_of rule permits a set materialized from the
customer's own column, which contains whatever the column contains — on the
fixture that includes a status spelled "Actve" — so accepting one unread would
enforce the misspelling forever rather than catch it. The second is that
everything is editable before it is accepted. The model wrote the name, the
description and the suggested severity; a person confirms them.

The values arrive only when a reviewer presses for one named proposal, which is
the same boundary the offending-rows panel sits on. See internal/api/proposals.go
for the other side of it.
*/

const SEVERITIES: Severity[] = ['error', 'warning', 'info']

export function Proposals({
  runId,
  datasetId,
  proposals,
}: {
  runId: string
  datasetId: string
  proposals: ProposalInfo[]
}) {
  // What this dataset already enforces. A proposal's id identifies what the
  // rule asserts rather than how it was worded, so an accepted rule keeps the
  // id of the proposal it came from and a reload can still tell the two apart.
  // Without this, a run opened a second time offers rules already in force and
  // the reviewer learns they are there by being refused.
  const [inForce, setInForce] = useState<Set<string>>(new Set())

  useEffect(() => {
    const ac = new AbortController()
    api
      .listDatasetRules(datasetId, ac.signal)
      .then((rules) => {
        if (!ac.signal.aborted) setInForce(new Set(rules.map((r) => r.id)))
      })
      .catch(() => {
        /* not knowing means offering the rule; the server refuses a duplicate */
      })
    return () => ac.abort()
  }, [datasetId])

  if (proposals.length === 0) {
    return <p className="empty">No rules were proposed for this dataset.</p>
  }

  return (
    <>
      <div className="notice">
        <p>
          These are expectations the model thinks should hold on this data every
          time it is audited, not just today. <strong>None is in force.</strong>{' '}
          Accept one and Veritix applies it to every future audit of this
          dataset, with no model involved — which is how something found once
          gets caught from then on.
        </p>
      </div>

      {proposals.map((p) => (
        <ProposalRow
          key={p.id}
          runId={runId}
          datasetId={datasetId}
          proposal={p}
          accepted={inForce.has(p.id)}
          onAccepted={() => setInForce((s) => new Set(s).add(p.id))}
        />
      ))}
    </>
  )
}

function ProposalRow({
  runId,
  datasetId,
  proposal,
  accepted,
  onAccepted,
}: {
  runId: string
  datasetId: string
  proposal: ProposalInfo
  accepted: boolean
  onAccepted: () => void
}) {
  const [expanded, setExpanded] = useState(false)
  const [detail, setDetail] = useState<ProposalDetail | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [result, setResult] = useState<api.Accepted | null>(null)

  // The reviewer's edits. The model authored all three; none of them is
  // enforced until a person has looked at it.
  const [name, setName] = useState(proposal.rule)
  const [description, setDescription] = useState(proposal.description ?? '')
  const [severity, setSeverity] = useState<Severity>(proposal.severity)
  const [struck, setStruck] = useState<Set<string>>(new Set())

  const hasValues = (proposal.permitted_value_count ?? 0) > 0

  async function review() {
    setLoading(true)
    setError('')
    try {
      setDetail(await api.getProposal(runId, proposal.id))
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  function strike(value: string) {
    setStruck((s) => {
      const next = new Set(s)
      if (next.has(value)) next.delete(value)
      else next.add(value)
      return next
    })
  }

  const values = detail?.proposal.permitted_values ?? []
  const kept = values.filter((v) => !struck.has(v))
  const nothingPermitted = hasValues && kept.length === 0

  async function accept() {
    setLoading(true)
    setError('')
    try {
      const req: api.AcceptRequest = {
        run_id: runId,
        proposal_id: proposal.id,
        id: name.trim(),
        description: description.trim(),
        // Always sent, never inherited: a rule that fails a build should do so
        // because somebody chose that, not because error is the default for a
        // rule a human wrote.
        severity,
      }
      if (hasValues) req.values = kept
      const out = await api.acceptProposal(datasetId, req)
      setResult(out)
      onAccepted()
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <article className={`proposal${result || accepted ? ' accepted' : ''}`}>
      <button
        className="proposal-head"
        onClick={() => setExpanded(!expanded)}
        aria-expanded={expanded}
      >
        <span className="badge plain">{proposal.expect}</span>
        <h4>{proposal.description || proposal.rule}</h4>
      </button>

      <div className="where">
        {proposal.target} · {proposal.rule}
        {(result || accepted) && ' · in force'}
      </div>

      <p className="sub num">
        {proposal.violations_now === 0
          ? 'Nothing breaks it today — an expectation that already holds.'
          : `${proposal.violations_now} row${proposal.violations_now === 1 ? '' : 's'} break${
              proposal.violations_now === 1 ? 's' : ''
            } it today.`}
        {hasValues && ` Permits the ${proposal.permitted_value_count} values the column holds.`}
      </p>

      {expanded && (
        <>
          {proposal.rationale && <p className="detail">{proposal.rationale}</p>}

          {result && (
            <p className="notice">
              In force as <strong>{result.rule.rule}</strong>. Every audit of this
              dataset applies it from now on — {result.rules_in_force} rule
              {result.rules_in_force === 1 ? '' : 's'} accepted so far. It is stored
              in this dataset's rules file, where it can be edited or removed.
            </p>
          )}

          {!result && accepted && (
            <p className="notice">
              A rule asserting this is already in force for this dataset, from an
              earlier audit.
            </p>
          )}

          {!result && !accepted && !detail && (
            <div className="gap-m">
              <button className="btn" onClick={review} disabled={loading}>
                {loading ? 'Fetching…' : hasValues ? 'Review the values and accept' : 'Review and accept'}
              </button>
              {hasValues && (
                <span className="sub hint">
                  Shows the values this rule would permit — actual values from your
                  data, for this rule only.
                </span>
              )}
            </div>
          )}

          {!result && detail && (
            <div className="accept gap-m">
              {hasValues && (
                <>
                  <div className="evidence-label">
                    The values this rule would permit. {detail.values_note}
                  </div>
                  <ul className="values">
                    {values.map((v) => (
                      <li key={v} className={struck.has(v) ? 'struck' : ''}>
                        <label className="check">
                          <input
                            type="checkbox"
                            checked={!struck.has(v)}
                            onChange={() => strike(v)}
                          />
                          {v === '' ? <span className="null">(blank)</span> : v}
                        </label>
                      </li>
                    ))}
                  </ul>
                  <p className="sub">
                    {struck.size === 0
                      ? 'Everything the column holds today is permitted, so nothing breaks this rule yet.'
                      : `${struck.size} struck out. Rows holding ${
                          struck.size === 1 ? 'it' : 'them'
                        } are reported from the next audit on — which is the point of striking one out.`}
                  </p>
                </>
              )}

              <div className="fields">
                <label>
                  Name
                  <input type="text" value={name} onChange={(e) => setName(e.target.value)} />
                </label>
                <label>
                  Report a violation as
                  <select
                    value={severity}
                    onChange={(e) => setSeverity(e.target.value as Severity)}
                  >
                    {SEVERITIES.map((s) => (
                      <option key={s} value={s}>
                        {s}
                      </option>
                    ))}
                  </select>
                </label>
                <label className="wide">
                  Description
                  <input
                    type="text"
                    value={description}
                    onChange={(e) => setDescription(e.target.value)}
                  />
                </label>
              </div>

              <details>
                <summary className="sub">The rule as it will be written</summary>
                <pre>{detail.yaml}</pre>
              </details>

              <div className="row">
                <button
                  className="btn primary"
                  onClick={accept}
                  disabled={loading || nothingPermitted || name.trim() === ''}
                >
                  {loading ? 'Accepting…' : 'Accept this rule'}
                </button>
                <button className="btn link" onClick={() => setDetail(null)}>
                  Cancel
                </button>
                {nothingPermitted && (
                  <span className="sub hint">
                    A rule permitting nothing would report every row. Leave at least
                    one value.
                  </span>
                )}
              </div>
            </div>
          )}

          {error && <p className="notice error">{error}</p>}
        </>
      )}
    </article>
  )
}
