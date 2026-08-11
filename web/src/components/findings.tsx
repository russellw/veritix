import { useEffect, useState } from 'react'

import * as api from '../api'
import type { Finding, FindingRows, Severity } from '../api'
import { count, share, where } from '../format'
import { navigate, onLinkClick } from '../router'

const ORDER: Severity[] = ['error', 'warning', 'info']
const HEADING: Record<Severity, string> = {
  error: 'Errors',
  warning: 'Warnings',
  info: 'Notes',
}

export function Findings({
  runId,
  findings,
  openId,
}: {
  runId: string
  findings: Finding[]
  openId?: string
}) {
  if (findings.length === 0) {
    return <p className="empty">No problems found.</p>
  }

  // The report already orders findings by severity; grouping here rather than
  // re-sorting keeps the screen and the downloaded report in the same order.
  const groups = ORDER.map((sev) => ({
    severity: sev,
    items: findings.filter((f) => f.severity === sev),
  })).filter((g) => g.items.length > 0)

  return (
    <>
      {groups.map((g) => (
        <section key={g.severity}>
          <h2>
            {HEADING[g.severity]} <span className="sub">({g.items.length})</span>
          </h2>
          {g.items.map((f) => (
            <FindingRow key={f.id} runId={runId} finding={f} open={f.id === openId} />
          ))}
        </section>
      ))}
    </>
  )
}

function FindingRow({
  runId,
  finding,
  open,
}: {
  runId: string
  finding: Finding
  open: boolean
}) {
  const [expanded, setExpanded] = useState(open)

  useEffect(() => {
    if (open) setExpanded(true)
  }, [open])

  function toggle() {
    const next = !expanded
    setExpanded(next)
    // The finding id is stable across runs, so the expanded state is worth
    // putting in the URL: the link still points at the same problem after a
    // re-run that turns up one more error alongside it.
    navigate(
      next ? `/runs/${runId}/findings/${finding.id}` : `/runs/${runId}`,
      true,
    )
  }

  return (
    <article className={`finding ${finding.severity}${open ? ' target' : ''}`}>
      <button
        className="finding-head"
        onClick={toggle}
        aria-expanded={expanded}
      >
        <span className={`badge ${finding.severity}`}>{finding.severity}</span>
        <h4>{finding.title}</h4>
      </button>

      <div className="where">
        {where(finding.source, finding.column)} · {finding.rule}
        {finding.origin === 'rule' && ' · your rule'}
        {finding.origin === 'agent' && ' · proposed by the model, verified'}
      </div>

      {expanded && (
        <>
          {finding.detail && <p className="detail">{finding.detail}</p>}
          {finding.remedy && <p className="remedy">{finding.remedy}</p>}

          {finding.affected_count !== undefined && (
            <p className="sub num">
              {count(finding.affected_count)}
              {finding.total ? ` of ${count(finding.total)} rows` : ' rows'}
              {finding.affected_share ? ` (${share(finding.affected_share)})` : ''}
            </p>
          )}
          {(finding.expected || finding.observed) && (
            <p className="sub">
              {finding.expected && <>expected {finding.expected}</>}
              {finding.expected && finding.observed && ' · '}
              {finding.observed && <>observed {finding.observed}</>}
            </p>
          )}

          {finding.evidence_query && (
            <>
              <div className="evidence-label">
                Evidence — this is the statement that produced the number above, so
                you can check it rather than take it on trust.
              </div>
              <pre>{finding.evidence_query}</pre>
            </>
          )}

          <Rows runId={runId} finding={finding} />

          <p className="sub gap-m">
            <a
              href={`/runs/${runId}/findings/${finding.id}`}
              onClick={onLinkClick(`/runs/${runId}/findings/${finding.id}`)}
            >
              Link to this finding
            </a>
          </p>
        </>
      )}
    </article>
  )
}

/*
Rows is the one place in the web interface where raw customer data appears.

It is behind an explicit press, one finding at a time, and it says so before it
fetches. Nothing here is prefetched and nothing is cached beyond the component:
close it and the values are gone from the page. See internal/api/rows.go for the
other half of the same boundary.
*/
function Rows({ runId, finding }: { runId: string; finding: Finding }) {
  const [rows, setRows] = useState<FindingRows | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  if (!finding.evidence_query) return null

  async function show() {
    setLoading(true)
    setError('')
    try {
      setRows(await api.getFindingRows(runId, finding.id))
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setLoading(false)
    }
  }

  if (rows) {
    return (
      <div className="gap-m">
        <div className="row">
          <span className="sub">
            The rows this finding is about — actual values from your data.
          </span>
          <button className="btn link" onClick={() => setRows(null)}>
            Hide
          </button>
        </div>
        <div className="scroll">
          <table>
            <thead>
              <tr>
                {rows.columns.map((c) => (
                  <th key={c}>{c}</th>
                ))}
              </tr>
            </thead>
            <tbody>
              {rows.rows.map((r, i) => (
                <tr key={i}>
                  {r.map((cell, j) => (
                    <td key={j}>{cell === null ? <span className="null">null</span> : cell}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        {rows.truncated && (
          <p className="sub">More rows matched than are shown here.</p>
        )}
      </div>
    )
  }

  return (
    <div className="gap-m">
      <button className="btn" onClick={show} disabled={loading}>
        {loading ? 'Fetching…' : 'Show the offending rows'}
      </button>
      <span className="sub hint">
        Reveals actual values from your data, for this finding only.
      </span>
      {error && <p className="notice error">{error}</p>}
    </div>
  )
}
