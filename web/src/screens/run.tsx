import { useCallback, useEffect, useRef, useState } from 'react'

import * as api from '../api'
import type { Report, Run } from '../api'
import { Findings } from '../components/findings'
import { Profile } from '../components/profile'
import { count, duration, when } from '../format'
import { onLinkClick } from '../router'

type Tab = 'findings' | 'tables'

export function RunScreen({ runId, findingId }: { runId: string; findingId?: string }) {
  const [run, setRun] = useState<Run | null>(null)
  const [report, setReport] = useState<Report | null>(null)
  const [progress, setProgress] = useState<string[]>([])
  const [error, setError] = useState('')
  const [tab, setTab] = useState<Tab>('findings')

  // Held in a ref so the stream effect does not restart every time a progress
  // line arrives.
  const streaming = useRef(false)

  const loadReport = useCallback(async (id: string, signal?: AbortSignal) => {
    try {
      setReport(await api.getReport(id, signal))
    } catch (e) {
      if (signal?.aborted) return
      // A failed or cancelled run legitimately has no report; that is not an
      // error to shout about, the run's own status already says what happened.
      setReport(null)
      if (e instanceof api.Unauthorized) throw e
    }
  }, [])

  useEffect(() => {
    const ac = new AbortController()
    setRun(null)
    setReport(null)
    setProgress([])
    setError('')
    streaming.current = false

    ;(async () => {
      try {
        const r = await api.getRun(runId, ac.signal)
        if (ac.signal.aborted) return
        setRun(r)

        if (r.status === 'succeeded') {
          await loadReport(runId, ac.signal)
          return
        }
        if (r.status === 'pending' || r.status === 'running') {
          streaming.current = true
          // The stream also delivers the terminal event for a run that has
          // already finished, so there is no race between starting a run and
          // subscribing to it.
          await api.streamRun(
            runId,
            (ev) => {
              if (ev.type === 'done' && ev.run) {
                setRun(ev.run)
                if (ev.run.status === 'succeeded') void loadReport(runId)
                return
              }
              if (ev.message) setProgress((p) => [...p, ev.message!])
            },
            ac.signal,
          )
        }
      } catch (e) {
        if (ac.signal.aborted) return
        setError(e instanceof Error ? e.message : String(e))
      }
    })()

    return () => ac.abort()
  }, [runId, loadReport])

  async function cancel() {
    try {
      setRun(await api.cancelRun(runId))
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  if (error) return <p className="notice error">{error}</p>
  if (!run) return <p className="empty">Loading…</p>

  const active = run.status === 'pending' || run.status === 'running'

  return (
    <>
      <h1>Audit</h1>
      <p className="sub">
        <a
          href={`/datasets/${run.dataset_id}`}
          onClick={onLinkClick(`/datasets/${run.dataset_id}`)}
        >
          ← the dataset
        </a>
        {' · '}
        {when(run.created_at)}
        {run.duration_ms > 0 && <> · {duration(run.duration_ms)}</>}
        {run.version && <> · {run.version}</>}
      </p>

      <p className="row">
        <span className={`status ${run.status}`}>{run.status}</span>
        {active && <span className="spinner" aria-label="running" />}
        {active && (
          <button className="btn danger" onClick={cancel}>
            Stop this audit
          </button>
        )}
        {run.status === 'succeeded' && (
          <a className="btn" href={api.reportDownloadURL(runId)} download>
            Download the report
          </a>
        )}
      </p>

      {run.message && run.status !== 'succeeded' && (
        <p className="notice error">{run.message}</p>
      )}

      {active && <Progress lines={progress} />}

      {report && (
        <>
          <div className="tiles">
            <div className="tile error">
              <div className="n">{report.finding_summary.errors}</div>
              <div className="k">Errors</div>
            </div>
            <div className="tile warning">
              <div className="n">{report.finding_summary.warnings}</div>
              <div className="k">Warnings</div>
            </div>
            <div className="tile info">
              <div className="n">{report.finding_summary.info}</div>
              <div className="k">Notes</div>
            </div>
            <div className="tile">
              <div className="n">{count(report.dataset.row_count)}</div>
              <div className="k">Rows</div>
            </div>
            <div className="tile">
              <div className="n">{report.dataset.table_count}</div>
              <div className="k">Tables</div>
            </div>
            {report.dataset.unreadable_rows > 0 && (
              <div className="tile error">
                <div className="n">{count(report.dataset.unreadable_rows)}</div>
                <div className="k">Unreadable rows</div>
              </div>
            )}
          </div>

          {/*
            Say what was withheld. The difference between "this column has no
            notable values" and "values were not included in this report" is the
            whole point of the redaction note.
          */}
          {report.redaction.note && <p className="notice">{report.redaction.note}</p>}

          <div className="tabs">
            <button
              className={tab === 'findings' ? 'current' : ''}
              onClick={() => setTab('findings')}
            >
              Findings ({report.finding_summary.total})
            </button>
            <button
              className={tab === 'tables' ? 'current' : ''}
              onClick={() => setTab('tables')}
            >
              Tables ({report.dataset.table_count})
            </button>
          </div>

          {tab === 'findings' ? (
            <Findings runId={runId} findings={report.findings ?? []} openId={findingId} />
          ) : (
            <Profile report={report} />
          )}
        </>
      )}
    </>
  )
}

/*
Progress lines are the pipeline's own log lines, streamed straight through. A
stage that is logged is a stage shown here, which is why there is no second list
of stage names in the front end to fall out of step with the back end.
*/
function Progress({ lines }: { lines: string[] }) {
  const end = useRef<HTMLLIElement>(null)

  useEffect(() => {
    end.current?.scrollIntoView({ block: 'nearest' })
  }, [lines.length])

  return (
    <div className="progress">
      <strong>Auditing…</strong>
      <ol>
        {lines.map((l, i) => (
          <li key={i} ref={i === lines.length - 1 ? end : null}>
            {l}
          </li>
        ))}
      </ol>
    </div>
  )
}
