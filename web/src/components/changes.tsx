import type { Comparison, FindingDelta, TableDelta } from '../api'
import { count, when, where } from '../format'
import { navigate, onLinkClick } from '../router'

/*
The comparison is the screen a business user comes back to. The first audit
tells them what is wrong; every one after it is really asking whether last
week's problems got fixed and whether anything new arrived, and a wall of
findings that looks identical week to week cannot answer either question.

So it appears twice and deliberately: a one-line strip beside the counts, which
is what somebody reads without deciding to, and a tab with the whole of it for
somebody who wants to know what moved. Both render the report's own comparison
field, so neither can disagree with the downloaded report.
*/

/** ChangeStrip is the glanceable half: what moved, in one line. */
export function ChangeStrip({
  comparison,
  onOpen,
}: {
  comparison: Comparison
  onOpen: () => void
}) {
  const s = comparison.summary
  const moved = s.new + s.worsened + s.resolved + s.improved

  return (
    <p className="change-strip">
      <span className="since">Since the previous audit ({when(comparison.baseline.started_at)}):</span>{' '}
      {moved === 0 ? (
        <span className="quiet">nothing changed</span>
      ) : (
        <>
          {s.new > 0 && <span className="worse">{s.new} new</span>}
          {s.worsened > 0 && <span className="worse">{s.worsened} worse</span>}
          {s.resolved > 0 && <span className="better">{s.resolved} resolved</span>}
          {s.improved > 0 && <span className="better">{s.improved} improved</span>}
        </>
      )}
      {(moved > 0 || (comparison.tables?.length ?? 0) > 0) && (
        <>
          {' '}
          <button className="btn link" onClick={onOpen}>
            what changed
          </button>
        </>
      )}
    </p>
  )
}

/** Changes is the tab: every finding that moved, and every table that drifted. */
export function Changes({
  runId,
  comparison,
  onOpenFinding,
}: {
  runId: string
  comparison: Comparison
  onOpenFinding: (id: string) => void
}) {
  const s = comparison.summary
  const findings = comparison.findings ?? []
  const tables = comparison.tables ?? []

  return (
    <div className="changes">
      <p className="sub">
        Compared with the audit of {when(comparison.baseline.started_at)}
        {comparison.baseline.run_id && (
          <>
            {' · '}
            <a
              href={`/runs/${comparison.baseline.run_id}`}
              onClick={onLinkClick(`/runs/${comparison.baseline.run_id}`)}
            >
              that audit
            </a>
          </>
        )}
      </p>

      <div className="tiles">
        <div className="tile error">
          <div className="n">{s.new}</div>
          <div className="k">New</div>
        </div>
        <div className="tile error">
          <div className="n">{s.worsened}</div>
          <div className="k">Worse</div>
        </div>
        <div className="tile info">
          <div className="n">{s.resolved}</div>
          <div className="k">Resolved</div>
        </div>
        <div className="tile info">
          <div className="n">{s.improved}</div>
          <div className="k">Improved</div>
        </div>
        <div className="tile">
          <div className="n">{s.unchanged}</div>
          <div className="k">Unchanged</div>
        </div>
      </div>

      {/*
        A note here is always a reason to trust the comparison less — most
        often a column that left the export, taking its findings with it and
        making them look fixed. It goes above the list it is about.
      */}
      {comparison.notes?.map((note) => (
        <p className="notice warn" key={note}>
          {note}
        </p>
      ))}

      {findings.length === 0 && tables.length === 0 && (
        <p className="empty">
          Nothing moved: the same findings, the same counts, the same tables.
        </p>
      )}

      {findings.length > 0 && (
        <ul className="change-list">
          {findings.map((f) => (
            <FindingChange key={f.id} runId={runId} delta={f} onOpen={onOpenFinding} />
          ))}
        </ul>
      )}

      {tables.length > 0 && (
        <>
          <h3>Tables</h3>
          {/*
            Volume and schema drift is the half of this no single audit can
            see. An export that quietly lost a third of its rows is a worse
            problem than anything in the findings, and it never appears as one.
          */}
          <ul className="change-list">
            {tables.map((t) => (
              <TableChange key={t.name} delta={t} />
            ))}
          </ul>
        </>
      )}
    </div>
  )
}

function FindingChange({
  runId,
  delta,
  onOpen,
}: {
  runId: string
  delta: FindingDelta
  onOpen: (id: string) => void
}) {
  const moved = delta.status === 'worsened' || delta.status === 'improved'
  const href = `/runs/${runId}/findings/${delta.id}`

  return (
    <li className={`change ${delta.status}`}>
      <span className={`badge ${badgeClass(delta.status)}`}>{delta.status}</span>
      {/*
        A resolved finding is not in this report, so there is nothing to open;
        everything else links to the same URL the findings list uses, since a
        finding's id names the problem and is worth sending to somebody.
      */}
      {delta.status === 'resolved' ? (
        <span className="title">{delta.title}</span>
      ) : (
        <a
          className="title"
          href={href}
          onClick={(e) => {
            if (e.defaultPrevented || e.button !== 0) return
            if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return
            e.preventDefault()
            // Both, in this order: the tab has to change as well as the URL,
            // or the link would update the address bar and show nothing.
            onOpen(delta.id)
            navigate(href)
          }}
        >
          {delta.title}
        </a>
      )}
      <div className="where">
        {where(delta.source ?? delta.table, delta.column)}
        {' · '}
        {delta.rule}
        {moved && (
          <>
            {' · '}
            {count(delta.affected_count_before)} → {count(delta.affected_count_after)} affected
          </>
        )}
        {delta.severity_before && (
          <>
            {' · '}
            {delta.severity_before} → {delta.severity}
          </>
        )}
      </div>
    </li>
  )
}

function TableChange({ delta }: { delta: TableDelta }) {
  const name = delta.source || delta.name
  return (
    <li className={`change ${delta.change}`}>
      <span className={`badge ${delta.change === 'removed' ? 'error' : 'plain'}`}>
        {delta.change}
      </span>
      <span className="title">{name}</span>
      <div className="where">
        {delta.change === 'added' && <>{count(delta.row_count_after)} rows</>}
        {delta.change === 'removed' && <>had {count(delta.row_count_before)} rows</>}
        {delta.change === 'changed' && (
          <>
            {delta.row_count_before !== delta.row_count_after && (
              <>
                {count(delta.row_count_before)} → {count(delta.row_count_after)} rows
              </>
            )}
            {delta.columns_removed?.length ? (
              <> · lost {delta.columns_removed.join(', ')}</>
            ) : null}
            {delta.columns_added?.length ? (
              <> · gained {delta.columns_added.join(', ')}</>
            ) : null}
          </>
        )}
      </div>
    </li>
  )
}

/** badgeClass colors a status by whether it is bad news. */
function badgeClass(status: FindingDelta['status']): string {
  switch (status) {
    case 'new':
    case 'worsened':
      return 'error'
    case 'resolved':
    case 'improved':
      return 'info'
    default:
      return 'plain'
  }
}
