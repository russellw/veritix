import type { Column, Report, Table } from '../api'
import { count, share } from '../format'

/*
The per-table profile, second to the findings.

A profile of a clean dataset is a wall of unremarkable numbers, and burying three
real problems inside it is how a tool gets ignored — so this is a separate tab,
not the landing view. The columns shown are the ones that make a reader suspect
something: what the column claims to be against what it holds, how much is
missing, and whether it is a key.
*/
export function Profile({ report }: { report: Report }) {
  const tables = report.tables ?? []
  if (tables.length === 0) return <p className="empty">Nothing was loaded.</p>

  return (
    <>
      {tables.map((t) => (
        <TableProfile key={t.name} table={t} />
      ))}

      {report.skipped_files && report.skipped_files.length > 0 && (
        <>
          <h2>Files not read</h2>
          <div className="scroll">
            <table>
              <thead>
                <tr>
                  <th>File</th>
                  <th>Why</th>
                </tr>
              </thead>
              <tbody>
                {report.skipped_files.map((s) => (
                  <tr key={s.file}>
                    <td className="mono">{s.file}</td>
                    <td>{s.reason}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </>
  )
}

function TableProfile({ table }: { table: Table }) {
  const r = table.reading
  return (
    <section>
      <h3>{table.source}</h3>
      <p className="sub">
        {count(table.row_count)} rows, {table.columns.length} columns
        {/*
          How the file was parsed is reported because a misdetected dialect or
          encoding invalidates everything downstream, and a reader needs to be
          able to check the assumption rather than trust it.
        */}
        {r && (
          <>
            {' · '}
            {r.format}
            {r.delimiter && `, delimiter ${r.delimiter}`}
            {r.encoding && `, ${r.encoding}`}
          </>
        )}
        {table.rejected_rows && (
          <span className="flag"> · {count(table.rejected_rows.count)} unreadable rows</span>
        )}
      </p>

      {table.notes?.map((n) => (
        <p key={n.code} className="notice">
          {n.message}
        </p>
      ))}

      <div className="scroll">
        <table>
          <thead>
            <tr>
              <th>Column</th>
              <th>Holds</th>
              <th className="n">Conforms</th>
              <th className="n">Missing</th>
              <th className="n">Distinct</th>
              <th>Key</th>
              <th>Shapes</th>
            </tr>
          </thead>
          <tbody>
            {table.columns.map((c) => (
              <ColumnRow key={c.name} column={c} />
            ))}
          </tbody>
        </table>
      </div>
    </section>
  )
}

function ColumnRow({ column: c }: { column: Column }) {
  // A column that mostly-but-not-quite matches a type is the interesting case:
  // it is where the stray "N/A" in a numeric column shows up.
  const imperfect = c.conformance > 0 && c.conformance < 1
  const missing = c.rows > 0 ? c.missing_total / c.rows : 0

  return (
    <tr>
      <td className="mono">
        {c.name}
        {c.original_name && <span className="sub"> ← {c.original_name}</span>}
      </td>
      <td>
        {c.inferred_type}
        {c.closest_type && (
          <span className="sub"> (nearest {c.closest_type}, {share(c.closest_match)})</span>
        )}
      </td>
      <td className={`n${imperfect ? ' flag' : ''}`}>
        {imperfect ? share(c.conformance) : c.conformance === 1 ? '100%' : '—'}
        {c.nonconforming_values > 0 && (
          <span className="sub"> ({count(c.nonconforming_values)} not)</span>
        )}
      </td>
      <td className={`n${missing > 0.1 ? ' flag' : ''}`}>
        {c.missing_total > 0 ? count(c.missing_total) : '—'}
      </td>
      <td className="n">{count(c.distinct)}</td>
      <td>{c.unique ? 'unique' : ''}</td>
      <td className="mono">
        {/*
          Shapes rather than values: CUS-000001 shows as XXX-999999, precise
          enough to reason about and useless for exfiltration. Verbatim values
          appear only when the run was asked for them.
        */}
        {(c.shapes ?? []).slice(0, 3).map((s) => s.value).join('  ')}
      </td>
    </tr>
  )
}
