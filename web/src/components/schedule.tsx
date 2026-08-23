import { useCallback, useEffect, useState } from 'react'

import * as api from '../api'
import type { Schedule } from '../api'
import { when } from '../format'
import { onLinkClick } from '../router'

const WEEKDAYS = [
  'sunday',
  'monday',
  'tuesday',
  'wednesday',
  'thursday',
  'friday',
  'saturday',
]

/** browserZone is the IANA zone this browser is in, or "" if it will not say. */
function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone ?? ''
  } catch {
    return ''
  }
}

/**
 * SchedulePanel is where somebody says "audit this every night".
 *
 * It is the half of the comparison that was missing: an audit nobody remembered
 * to start says nothing about what changed since the last one, and the people
 * this interface is for are not going to open it every morning.
 *
 * Uploaded datasets do not get one. An upload is a copy of the data as it was,
 * so auditing it nightly would produce the same report forever and a comparison
 * that never said anything — which looks exactly like a schedule that works.
 */
export function SchedulePanel({
  datasetId,
  uploaded,
  clockRunning,
  notifyConfigured,
}: {
  datasetId: string
  uploaded: boolean
  clockRunning: boolean
  notifyConfigured: boolean
}) {
  const [saved, setSaved] = useState<Schedule | null>(null)
  const [kind, setKind] = useState<'off' | 'daily' | 'weekly' | 'interval'>('off')
  const [at, setAt] = useState('02:00')
  const [weekday, setWeekday] = useState('sunday')
  const [hours, setHours] = useState(6)
  const [zone, setZone] = useState(browserZone())
  const [notify, setNotify] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const load = useCallback(
    async (signal?: AbortSignal) => {
      try {
        const s = await api.getSchedule(datasetId, signal)
        if (signal?.aborted) return
        setSaved(s)
        setKind(s.kind)
        if (s.at) setAt(s.at)
        if (s.weekday) setWeekday(s.weekday)
        if (s.every_minutes) setHours(Math.max(1, Math.round(s.every_minutes / 60)))
        if (s.timezone) setZone(s.timezone === 'Local' ? '' : s.timezone)
        setNotify(s.notify)
      } catch {
        // Most datasets are not audited on a schedule, and the 404 that says
        // so is an ordinary answer rather than something to report.
        if (!signal?.aborted) setSaved(null)
      }
    },
    [datasetId],
  )

  useEffect(() => {
    const ac = new AbortController()
    setSaved(null)
    setKind('off')
    setError('')
    void load(ac.signal)
    return () => ac.abort()
  }, [load])

  if (uploaded) return null

  async function save() {
    setBusy(true)
    setError('')
    try {
      if (kind === 'off') {
        await api.deleteSchedule(datasetId)
        setSaved(null)
      } else {
        const body: Schedule = { kind, notify, timezone: zone }
        if (kind === 'daily' || kind === 'weekly') body.at = at
        if (kind === 'weekly') body.weekday = weekday
        if (kind === 'interval') body.every_minutes = Math.round(hours * 60)
        setSaved(await api.setSchedule(datasetId, body))
      }
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <h2>Audit on a schedule</h2>
      <p className="sub">
        Veritix starts the audit itself and compares it with the one before, so
        that somebody finds out when the export gets worse without having to
        remember to look.
      </p>

      <div className="fields">
        <label>
          How often
          <select
            aria-label="How often"
            value={kind}
            onChange={(e) => setKind(e.target.value as typeof kind)}
          >
            <option value="off">Never — only when I ask</option>
            <option value="daily">Every day</option>
            <option value="weekly">Every week</option>
            <option value="interval">Every few hours</option>
          </select>
        </label>

        {kind === 'weekly' && (
          <label>
            On
            <select
              aria-label="Day of the week"
              value={weekday}
              onChange={(e) => setWeekday(e.target.value)}
            >
              {WEEKDAYS.map((d) => (
                <option key={d} value={d}>
                  {d[0].toUpperCase() + d.slice(1)}
                </option>
              ))}
            </select>
          </label>
        )}

        {(kind === 'daily' || kind === 'weekly') && (
          <label>
            At
            <input
              type="time"
              aria-label="Time of day"
              value={at}
              onChange={(e) => setAt(e.target.value)}
            />
          </label>
        )}

        {kind === 'interval' && (
          <label>
            Hours between audits
            <input
              type="number"
              aria-label="Hours between audits"
              min={1}
              max={168}
              value={hours}
              onChange={(e) => setHours(Number(e.target.value))}
            />
          </label>
        )}

        {(kind === 'daily' || kind === 'weekly') && (
          <label className="wide">
            Time zone
            <input
              type="text"
              aria-label="Time zone"
              className="mono"
              placeholder="the server's own"
              value={zone}
              onChange={(e) => setZone(e.target.value)}
            />
          </label>
        )}
      </div>

      <p className="row gap-l">
        {kind !== 'off' && notifyConfigured && (
          <label className="check">
            <input
              type="checkbox"
              checked={notify}
              onChange={(e) => setNotify(e.target.checked)}
            />
            Tell me when it gets worse
          </label>
        )}
        <span className="spacer" />
        <button className="btn" onClick={save} disabled={busy}>
          {busy ? 'Saving…' : kind === 'off' ? 'Turn off' : 'Save schedule'}
        </button>
      </p>

      {(kind === 'daily' || kind === 'weekly') && (
        <p className="sub">
          Read in {zone || "the server's own time zone"}. On the night the
          clocks go forward there may be no such time, and the audit runs at the
          next moment there is; on the night they go back the hour happens twice
          and it runs once.
        </p>
      )}

      {error && <p className="notice error">{error}</p>}

      {saved && (
        <p className="sub" data-testid="schedule-state">
          Next audit {when(saved.next_due_at)}
          {saved.notify && ' · you will be told about regressions'}
          {saved.last_run_id && (
            <>
              {' · last one '}
              <a
                href={`/runs/${saved.last_run_id}`}
                onClick={onLinkClick(`/runs/${saved.last_run_id}`)}
              >
                ran
              </a>
            </>
          )}
        </p>
      )}
      {saved?.last_error && (
        <p className="notice">The last window did not start an audit: {saved.last_error}</p>
      )}
      {saved && !clockRunning && (
        <p className="notice">
          This server is not running the clock, so nothing will fire this
          schedule. It is stored, and another Veritix using the same data
          directory can run it.
        </p>
      )}
    </>
  )
}
