import { useCallback, useEffect, useState } from 'react'

import * as api from '../api'
import type { Dataset, Run } from '../api'
import { count, duration, when } from '../format'
import { navigate, onLinkClick } from '../router'

export function DatasetScreen({
  datasetId,
  onChanged,
}: {
  datasetId: string
  onChanged: () => void
}) {
  const [dataset, setDataset] = useState<Dataset | null>(null)
  const [runs, setRuns] = useState<Run[]>([])
  const [error, setError] = useState('')
  const [starting, setStarting] = useState(false)
  const [includeValues, setIncludeValues] = useState(false)

  const load = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const [d, r] = await Promise.all([
          api.getDataset(datasetId, signal),
          api.listRuns(datasetId, signal),
        ])
        if (signal?.aborted) return
        setDataset(d)
        setRuns(r)
      } catch (e) {
        if (signal?.aborted) return
        setError(e instanceof Error ? e.message : String(e))
      }
    },
    [datasetId],
  )

  useEffect(() => {
    const ac = new AbortController()
    setDataset(null)
    setRuns([])
    setError('')
    void load(ac.signal)
    return () => ac.abort()
  }, [load])

  async function start() {
    setStarting(true)
    setError('')
    try {
      const run = await api.startRun({ dataset_id: datasetId, include_values: includeValues })
      navigate(`/runs/${run.id}`)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
      setStarting(false)
    }
  }

  async function forget() {
    if (!dataset) return
    const uploaded = dataset.uploaded
    const message = uploaded
      ? `Forget ${dataset.name}? The files Veritix is holding, and this dataset's audit history, are deleted.`
      : `Forget ${dataset.name}? Its audit history is deleted. Your own files at ${dataset.path} are left alone.`
    if (!window.confirm(message)) return

    try {
      await api.deleteDataset(datasetId)
      onChanged()
      navigate('/')
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    }
  }

  if (error && !dataset) return <p className="notice error">{error}</p>
  if (!dataset) return <p className="empty">Loading…</p>

  return (
    <>
      <h1>{dataset.name}</h1>
      <p className="sub mono">{dataset.path}</p>
      <p className="sub">
        {dataset.uploaded ? 'Uploaded to this server' : 'Read in place from this server'} ·
        added {when(dataset.created_at)}
      </p>

      <p className="row gap-l">
        <button className="btn primary" onClick={start} disabled={starting}>
          {starting ? 'Starting…' : 'Run an audit'}
        </button>
        <label className="check">
          <input
            type="checkbox"
            checked={includeValues}
            onChange={(e) => setIncludeValues(e.target.checked)}
          />
          Include cell values in the report
        </label>
        <span className="spacer" />
        <button className="btn danger" onClick={forget}>
          Forget this dataset
        </button>
      </p>
      {includeValues && (
        <p className="notice">
          The report will contain verbatim values from your data. A report is a file
          that gets emailed and pasted into tickets — leave this off unless you
          need the examples.
        </p>
      )}
      {error && <p className="notice error">{error}</p>}

      <h2>Audits</h2>
      {runs.length === 0 ? (
        <p className="empty">Nothing has been audited yet.</p>
      ) : (
        <div className="scroll">
          <table>
            <thead>
              <tr>
                <th>When</th>
                <th>Status</th>
                <th className="n">Errors</th>
                <th className="n">Warnings</th>
                <th className="n">Notes</th>
                <th className="n">Took</th>
              </tr>
            </thead>
            <tbody>
              {runs.map((r) => (
                <tr key={r.id}>
                  <td>
                    <a href={`/runs/${r.id}`} onClick={onLinkClick(`/runs/${r.id}`)}>
                      {when(r.created_at)}
                    </a>
                  </td>
                  <td>
                    <span className={`status ${r.status}`}>{r.status}</span>
                    {r.message && r.status !== 'succeeded' && (
                      <div className="sub">{r.message}</div>
                    )}
                  </td>
                  <td className="n">{count(r.findings.errors)}</td>
                  <td className="n">{count(r.findings.warnings)}</td>
                  <td className="n">{count(r.findings.info)}</td>
                  <td className="n">{duration(r.duration_ms)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  )
}
