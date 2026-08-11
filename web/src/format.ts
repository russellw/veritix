/** Small formatters, shared so that two screens cannot render a count differently. */

export function count(n: number | undefined): string {
  if (n === undefined) return '—'
  return n.toLocaleString()
}

export function duration(ms: number | undefined): string {
  if (!ms) return '—'
  if (ms < 1000) return `${ms} ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)} s`
  const m = Math.floor(ms / 60_000)
  const s = Math.round((ms % 60_000) / 1000)
  return `${m}m ${s}s`
}

export function when(iso: string | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return '—'
  return d.toLocaleString(undefined, {
    day: 'numeric',
    month: 'short',
    hour: '2-digit',
    minute: '2-digit',
  })
}

export function share(v: number | undefined): string {
  if (v === undefined || v === 0) return '—'
  if (v < 0.001) return '<0.1%'
  return `${(v * 100).toFixed(v < 0.1 ? 1 : 0)}%`
}

/** where renders a finding's location the way the text and HTML reports do. */
export function where(source?: string, column?: string): string {
  if (!source) return column ?? ''
  return column ? `${source}.${column}` : source
}
