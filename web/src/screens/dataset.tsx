import { useCallback, useEffect, useState } from 'react'

import * as api from '../api'
import type { Capabilities, Dataset, ProposalInfo, Run } from '../api'
import { count, duration, when } from '../format'
import { SchedulePanel } from '../components/schedule'
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
  const [rules, setRules] = useState<ProposalInfo[]>([])
  const [error, setError] = useState('')
  const [starting, setStarting] = useState(false)
  const [includeValues, setIncludeValues] = useState(false)
  const [useAgent, setUseAgent] = useState(false)
  const [sendValues, setSendValues] = useState(false)
  const [caps, setCaps] = useState<Capabilities | null>(null)

  const load = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const [d, r, rl] = await Promise.all([
          api.getDataset(datasetId, signal),
          api.listRuns(datasetId, signal),
          api.listDatasetRules(datasetId, signal),
        ])
        if (signal?.aborted) return
        setDataset(d)
        setRuns(r)
        setRules(rl)
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
    setRules([])
    setError('')
    void load(ac.signal)
    return () => ac.abort()
  }, [load])

  // The agentic audit is offered only where a model is actually configured.
  // Showing a control that fails when it is used is worse than not showing it.
  useEffect(() => {
    const ac = new AbortController()
    api
      .getCapabilities(ac.signal)
      .then((c) => {
        if (!ac.signal.aborted) setCaps(c)
      })
      .catch(() => {
        /* a server that will not say assumes no model, which is the safe read */
      })
    return () => ac.abort()
  }, [])

  async function start() {
    setStarting(true)
    setError('')
    try {
      const run = await api.startRun({
        dataset_id: datasetId,
        include_values: includeValues,
        agent: useAgent,
        allow_sample_values: useAgent && sendValues,
      })
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
        {caps?.agent.available && (
          <label className="check">
            <input
              type="checkbox"
              checked={useAgent}
              onChange={(e) => setUseAgent(e.target.checked)}
            />
            Also investigate with {caps.agent.model || caps.agent.provider}
          </label>
        )}
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
      {useAgent && (
        <div className="notice">
          <p>
            The model will be sent this dataset's structure and measurements —
            column names, counts, distributions, and value shapes such as
            XXX-999999 — and nothing out of any row. It proposes findings; each
            one is only reported if Veritix can reproduce it against your data.
            Afterwards you can read every payload that left this machine.
          </p>
          <label className="check">
            <input
              type="checkbox"
              checked={sendValues}
              onChange={(e) => setSendValues(e.target.checked)}
            />
            Let it see cell values too, with obvious identifiers masked
          </label>
          {sendValues && (
            <p className="sub">
              Your data will be sent to {caps?.agent.provider}. Leave this off
              unless the shapes are not telling you enough.
            </p>
          )}
        </div>
      )}
      {error && <p className="notice error">{error}</p>}

      {/*
        What this dataset enforces on its own account, which is where an
        accepted proposal ends up. It is above the audit history rather than
        below it because it applies to the next audit as much as the last one:
        a person about to press "Run an audit" should be able to see what will
        be checked without a model.
      */}
      {rules.length > 0 && (
        <>
          <h2>Rules in force</h2>
          <p className="sub">
            Accepted from what a model proposed. Every audit of this dataset
            applies them, with no model involved. They are shown as they are
            written in this dataset's rules file, which is where they can be
            edited or removed.
          </p>
          <ul className="rule-list">
            {rules.map((r) => (
              <li key={r.id}>
                <span className="badge plain">{r.expect}</span>
                <span>{r.description || r.rule}</span>
                <span className="sub">
                  {r.target} · {r.rule} · {r.severity}
                </span>
              </li>
            ))}
          </ul>
        </>
      )}

      <SchedulePanel
        datasetId={datasetId}
        uploaded={dataset.uploaded}
        clockRunning={caps?.schedule?.available ?? true}
        notifyConfigured={caps?.schedule?.notify ?? false}
      />

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
