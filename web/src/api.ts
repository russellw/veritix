/*
The typed client for /api/v1. Shapes mirror internal/api/openapi.yaml, which is
the contract — settle a change there first.

Every request is same-origin: in production the Go binary serves this bundle and
the API, and in development Vite proxies /api to `veritix serve`. Nothing here
may ever address another host. That is not a style preference — the CSP served
alongside this bundle sets `connect-src 'self'`, so a fetch to anywhere else is
refused by the browser, and this file is written to match.
*/

// ── shapes ───────────────────────────────────────────────────────────────

export type Severity = 'error' | 'warning' | 'info'

export type RunStatus = 'pending' | 'running' | 'succeeded' | 'failed' | 'canceled'

export interface Dataset {
  id: string
  name: string
  path: string
  uploaded: boolean
  created_at: string
}

export interface FindingCounts {
  total: number
  errors: number
  warnings: number
  info: number
}

export interface Run {
  id: string
  dataset_id: string
  status: RunStatus
  message?: string
  version?: string
  created_at: string
  started_at?: string
  finished_at?: string
  duration_ms: number
  findings: FindingCounts
}

export interface Finding {
  id: string
  rule: string
  severity: Severity
  origin: 'check' | 'rule' | 'agent'
  title: string
  detail?: string
  remedy?: string
  table?: string
  source?: string
  column?: string
  file?: string
  line?: number
  affected_count?: number
  total?: number
  affected_share?: number
  expected?: string
  observed?: string
  evidence_query?: string
  verified: boolean
}

export interface ValueInfo {
  value: string
  count: number
  share: number
}

export interface Column {
  name: string
  original_name?: string
  position: number
  inferred_type: string
  declared_type?: string
  conformance: number
  nonconforming_values: number
  closest_type?: string
  closest_match?: number
  rows: number
  nulls: number
  blanks: number
  missing_total: number
  distinct: number
  distinct_normalized: number
  unique: boolean
  min_length: number
  max_length: number
  avg_length: number
  leading_whitespace: number
  trailing_whitespace: number
  sentinels?: ValueInfo[]
  shapes?: ValueInfo[]
  top_values?: ValueInfo[]
}

export interface Reading {
  format: string
  delimiter?: string
  quote?: string
  encoding?: string
  has_header: boolean
  skip_rows?: number
  header_row?: number
}

export interface Note {
  code: string
  message: string
}

export interface Table {
  name: string
  source: string
  file: string
  sheet?: string
  row_count: number
  columns: Column[]
  reading?: Reading
  rejected_rows?: { count: number; samples?: unknown[] }
  notes?: Note[]
}

/**
 * AgentInfo summarizes the model-driven investigation, when one ran.
 *
 * Absent from a deterministic-only audit rather than present and empty, so the
 * interface can tell at a glance whether a model was involved at all.
 */
export interface AgentInfo {
  provider: string
  model: string
  steps: number
  tool_calls: number
  findings: number
  not_reproduced: number
  input_tokens: number
  output_tokens: number
  values_sent_to_model: boolean
  values_withheld: number
  context_documents?: number
  context_servers?: string[]
  stopped: 'finished' | 'step_budget' | 'token_budget' | 'provider_error' | 'refused' | 'canceled'
  complete: boolean
  duration_ms: number
}

/**
 * ProposalInfo is one rule the model suggested, as everything that lists rules
 * describes them: the shape of the expectation and never its contents.
 *
 * A one_of rule's permitted set is materialized from the customer's own column,
 * so it is cell values, and it is absent here by design —
 * `permitted_value_count` says how many there are and `getProposal` is the one
 * call that fetches them. The same shape describes a rule already in force, so
 * `GET /datasets/{id}/rules` returns these too.
 */
export interface ProposalInfo {
  id: string
  rule: string
  description?: string
  rationale?: string
  target: string
  table: string
  column?: string
  expect: string
  severity: Severity
  violations_now: number
  permitted_value_count?: number
  /** Present only where the caller has asked for one proposal by name. */
  permitted_values?: string[]
}

/** ProposalDetail is one proposal, with the values it would permit. */
export interface ProposalDetail {
  proposal: ProposalInfo
  /** The rule as it would be written in a rules file. */
  yaml: string
  values_note?: string
}

/**
 * What changed since the previous successful audit of the same dataset.
 *
 * Nothing asks for it: a run carries one when there is something to compare
 * against, and a dataset's first audit does not. This is the question a
 * business user actually has on every audit after the first — three errors is
 * a number, three errors that were two last week is a direction.
 */
export type DeltaStatus = 'new' | 'worsened' | 'resolved' | 'improved' | 'unchanged'

export interface FindingDelta {
  id: string
  rule: string
  status: DeltaStatus
  severity: Severity
  /** Set only when the severity moved, which a count cannot show. */
  severity_before?: Severity
  title: string
  table?: string
  source?: string
  column?: string
  affected_count_before: number
  affected_count_after: number
}

export interface TableDelta {
  name: string
  source?: string
  change: 'added' | 'removed' | 'changed'
  row_count_before: number
  row_count_after: number
  columns_added?: string[]
  columns_removed?: string[]
}

export interface Comparison {
  baseline: {
    run_id?: string
    source?: string
    started_at: string
    veritix_version?: string
    root?: string
  }
  summary: {
    new: number
    worsened: number
    resolved: number
    improved: number
    unchanged: number
    new_errors: number
    new_warnings: number
  }
  /**
   * Only what moved. An unchanged finding is counted in the summary and left
   * out here, because it is already in the report's own findings list.
   */
  findings?: FindingDelta[]
  tables?: TableDelta[]
  notes?: string[]
}

export interface Report {
  schema: string
  veritix_version?: string
  run: { started_at: string; duration_ms: number }
  dataset: {
    root: string
    file_count: number
    table_count: number
    column_count: number
    row_count: number
    unreadable_rows: number
  }
  finding_summary: FindingCounts
  findings?: Finding[]
  comparison?: Comparison
  rule_proposals?: ProposalInfo[]
  tables?: Table[]
  skipped_files?: { file: string; reason: string }[]
  warnings?: Note[]
  redaction: { values_included: boolean; note?: string }
  agent?: AgentInfo
}

// ── the agent's trace ────────────────────────────────────────────────────

export interface TraceCall {
  tool: string
  arguments?: unknown
  /** The exact bytes sent back to the model. */
  result?: string
  is_error?: boolean
  duration_ms: number
}

export interface TraceStep {
  step: number
  thinking?: string
  text?: string
  calls?: TraceCall[]
  stop_reason?: string
  usage: TokenUsage
  duration_ms: number
}

export interface TokenUsage {
  input_tokens: number
  output_tokens: number
  cache_read_tokens?: number
  cache_write_tokens?: number
  reasoning_tokens?: number
}

/**
 * AgentTrace is the record of what the model was told and what it answered.
 *
 * It is the artifact that makes the egress promise checkable rather than
 * merely stated, which is why the interface shows every payload verbatim
 * instead of summarizing it.
 */
export interface AgentTrace {
  provider: string
  model: string
  steps: TraceStep[]
  usage: TokenUsage
  redaction: {
    shaped: number
    masked: number
    passed: number
    truncated: number
    sealed: number
    bytes: number
    context_documents?: number
    context_bytes?: number
  }
  values_allowed: boolean
  context?: ContextTrace
  findings: number
  not_reproduced: number
  max_steps: number
  token_budget?: number
  stopped: AgentInfo['stopped']
  error?: string
  started_at: string
  duration_ms: number
}

/**
 * ContextTrace is the outbound half of the same record: what Veritix asked the
 * customer's own MCP servers for, and what it got back.
 *
 * The trace has always answered "what was the model sent". A context server is
 * the first thing since the model that anything leaves the process toward, so
 * the screen has to answer "what did Veritix send, and to whom" as well —
 * which is why every request is listed rather than counted.
 */
export interface ContextTrace {
  servers: { name: string; documents: number; omitted?: number; error?: string }[]
  documents?: {
    id: string
    server: string
    name: string
    description?: string
    mime_type?: string
    size_bytes?: number
  }[]
  requests?: {
    server: string
    method: string
    uri?: string
    bytes?: number
    duration_ms: number
    error?: string
  }[]
  documents_read: number
  bytes_admitted: number
}

/** Capabilities is what this server can do, which shapes what is offered. */
export interface Capabilities {
  agent: {
    available: boolean
    provider?: string
    model?: string
    values_allowed_by_default?: boolean
  }
}

/** FindingRows is the one response that carries raw customer data. */
export interface FindingRows {
  finding_id: string
  title?: string
  columns: string[]
  rows: (string | null)[][]
  truncated: boolean
}

export interface ProgressEvent {
  seq?: number
  type: 'progress' | 'done'
  time: string
  message?: string
  fields?: Record<string, unknown>
  run?: Run
}

// ── auth ─────────────────────────────────────────────────────────────────

/*
The token lives in sessionStorage rather than localStorage: it is a credential
for a server that can read the customer's files, and it should not outlive the
tab it was typed into. A loopback server started without a token needs none of
this, which is the common case.
*/
const TOKEN_KEY = 'veritix.token'

export function authToken(): string {
  return sessionStorage.getItem(TOKEN_KEY) ?? ''
}

export function setAuthToken(token: string) {
  if (token) sessionStorage.setItem(TOKEN_KEY, token)
  else sessionStorage.removeItem(TOKEN_KEY)
}

/** Unauthorized is thrown on a 401 so the shell can ask for a token. */
export class Unauthorized extends Error {
  constructor() {
    super('this server requires a bearer token')
    this.name = 'Unauthorized'
  }
}

function headers(extra?: Record<string, string>): Record<string, string> {
  const h: Record<string, string> = { ...extra }
  const token = authToken()
  if (token) h['Authorization'] = `Bearer ${token}`
  return h
}

// ── requests ─────────────────────────────────────────────────────────────

const BASE = '/api/v1'

async function fail(res: Response): Promise<never> {
  if (res.status === 401) throw new Unauthorized()

  // The API returns one error shape for everything, but a proxy in front of it
  // might not, so a body that will not parse falls back to the status line.
  let message = `${res.status} ${res.statusText}`
  try {
    const body = (await res.json()) as { error?: string }
    if (body.error) message = body.error
  } catch {
    /* keep the status line */
  }
  throw new Error(message)
}

async function get<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(BASE + path, { headers: headers(), signal })
  if (!res.ok) return fail(res)
  return (await res.json()) as T
}

async function send<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(BASE + path, {
    method,
    headers: headers(body === undefined ? {} : { 'Content-Type': 'application/json' }),
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) return fail(res)
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

export interface Health {
  status: string
  version: string
  /* Where this build's source can be had. The footer offers it; see app.tsx. */
  source_url: string
}

export async function health(): Promise<Health> {
  const res = await fetch(`${BASE}/health`)
  if (!res.ok) return fail(res)
  return (await res.json()) as Health
}

export async function listDatasets(signal?: AbortSignal): Promise<Dataset[]> {
  const body = await get<{ datasets: Dataset[] }>('/datasets', signal)
  return body.datasets ?? []
}

export function getDataset(id: string, signal?: AbortSignal): Promise<Dataset> {
  return get<Dataset>(`/datasets/${encodeURIComponent(id)}`, signal)
}

export function registerDataset(path: string, name?: string): Promise<Dataset> {
  return send<Dataset>('POST', '/datasets', name ? { path, name } : { path })
}

export async function uploadDataset(name: string, files: File[]): Promise<Dataset> {
  const form = new FormData()
  form.append('name', name)
  // The field name is fixed by the contract; the server takes base names only,
  // which is what makes a browser's folder upload (relative paths in the
  // filename) safe to accept.
  for (const f of files) form.append('files', f, f.name)

  const res = await fetch(`${BASE}/datasets`, { method: 'POST', headers: headers(), body: form })
  if (!res.ok) return fail(res)
  return (await res.json()) as Dataset
}

export function deleteDataset(id: string): Promise<void> {
  return send<void>('DELETE', `/datasets/${encodeURIComponent(id)}`)
}

export async function listRuns(datasetId?: string, signal?: AbortSignal): Promise<Run[]> {
  const q = datasetId ? `?dataset_id=${encodeURIComponent(datasetId)}` : ''
  const body = await get<{ runs: Run[] }>(`/runs${q}`, signal)
  return body.runs ?? []
}

export function getRun(id: string, signal?: AbortSignal): Promise<Run> {
  return get<Run>(`/runs/${encodeURIComponent(id)}`, signal)
}

export interface RunRequest {
  dataset_id: string
  include_values?: boolean
  top_values?: number
  rules?: string
  /** Run the model-driven investigation as well as the deterministic checks. */
  agent?: boolean
  /** Permit the model to see cell values, masked, for this run alone. */
  allow_sample_values?: boolean
}

export function startRun(req: RunRequest): Promise<Run> {
  return send<Run>('POST', '/runs', req)
}

export function cancelRun(id: string): Promise<Run> {
  return send<Run>('POST', `/runs/${encodeURIComponent(id)}/cancel`)
}

export function getReport(id: string, signal?: AbortSignal): Promise<Report> {
  return get<Report>(`/runs/${encodeURIComponent(id)}/report`, signal)
}

export function getCapabilities(signal?: AbortSignal): Promise<Capabilities> {
  return get<Capabilities>('/capabilities', signal)
}

/**
 * getTrace fetches what the model was sent and what it answered.
 *
 * 404 means the run was audited without a model, which is an ordinary answer
 * rather than a failure — the caller distinguishes the two by the run it
 * already has.
 */
export function getTrace(runId: string, signal?: AbortSignal): Promise<AgentTrace> {
  return get<AgentTrace>(`/runs/${encodeURIComponent(runId)}/trace`, signal)
}

/**
 * getFindingRows fetches the rows a finding is about.
 *
 * This is the only call in the client that returns raw cell values. It is
 * deliberately not folded into getReport and is never issued eagerly: the user
 * asks for one finding's rows, and only then does customer data cross into the
 * browser. Keep it that way.
 */
export function getFindingRows(
  runId: string,
  findingId: string,
  limit = 50,
  signal?: AbortSignal,
): Promise<FindingRows> {
  return get<FindingRows>(
    `/runs/${encodeURIComponent(runId)}/findings/${encodeURIComponent(findingId)}/rows?limit=${limit}`,
    signal,
  )
}

/**
 * getProposal fetches one proposed rule, including the values it would permit.
 *
 * This is the second of the two calls that return raw cell values, and it is
 * gated the same way as the first: one named proposal at a time, only when a
 * reviewer opens it. The values are the whole of the review — a vocabulary
 * materialized from a column contains whatever the column contains, misspelling
 * included, and accepting one unread would enforce the misspelling forever. See
 * internal/api/proposals.go.
 */
export function getProposal(
  runId: string,
  proposalId: string,
  signal?: AbortSignal,
): Promise<ProposalDetail> {
  return get<ProposalDetail>(
    `/runs/${encodeURIComponent(runId)}/proposals/${encodeURIComponent(proposalId)}`,
    signal,
  )
}

export interface AcceptRequest {
  run_id: string
  proposal_id: string
  /** Rename the rule. It has to be unique among the rules in force. */
  id?: string
  description?: string
  severity?: Severity
  /** The permitted set as the reviewer left it, for a one_of rule. */
  values?: string[]
}

export interface Accepted {
  rule: ProposalInfo
  rules_in_force: number
}

/**
 * acceptProposal writes a rule into the dataset's own rules file.
 *
 * From here on every audit of this dataset applies it, with no model involved,
 * which is the point of proposing rules at all. Nothing is automatic and
 * nothing is retroactive: this is a person deciding.
 */
export function acceptProposal(datasetId: string, req: AcceptRequest): Promise<Accepted> {
  return send<Accepted>('POST', `/datasets/${encodeURIComponent(datasetId)}/rules`, req)
}

/** listDatasetRules is what has been accepted for a dataset so far. */
export async function listDatasetRules(
  datasetId: string,
  signal?: AbortSignal,
): Promise<ProposalInfo[]> {
  const body = await get<{ rules: ProposalInfo[] }>(
    `/datasets/${encodeURIComponent(datasetId)}/rules`,
    signal,
  )
  return body.rules ?? []
}

/** reportDownloadURL is the self-contained HTML report, for a normal download. */
export function reportDownloadURL(runId: string): string {
  return `${BASE}/runs/${encodeURIComponent(runId)}/report.html`
}

// ── events ───────────────────────────────────────────────────────────────

/**
 * streamRun delivers a run's progress events.
 *
 * This parses the SSE framing over fetch rather than using EventSource, because
 * EventSource cannot send an Authorization header — and the alternative, a token
 * in the query string, would put a credential somewhere it gets copied, logged
 * and shoulder-read. The framing is four lines of parsing; the credential
 * handling is not something to compromise on.
 */
export async function streamRun(
  runId: string,
  onEvent: (ev: ProgressEvent) => void,
  signal: AbortSignal,
): Promise<void> {
  const res = await fetch(`${BASE}/runs/${encodeURIComponent(runId)}/events`, {
    headers: headers({ Accept: 'text/event-stream' }),
    signal,
  })
  if (!res.ok) return fail(res)
  if (!res.body) throw new Error('this browser cannot read the event stream')

  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader()
  let buffer = ''

  for (;;) {
    const { done, value } = await reader.read()
    if (done) return
    buffer += value

    // Events are separated by a blank line. Anything after the last separator
    // is a partial event still arriving, so it stays in the buffer.
    let split: number
    while ((split = buffer.indexOf('\n\n')) !== -1) {
      const chunk = buffer.slice(0, split)
      buffer = buffer.slice(split + 2)

      const data = chunk
        .split('\n')
        .filter((l) => l.startsWith('data:'))
        .map((l) => l.slice(5).trim())
        .join('\n')
      if (!data) continue

      try {
        onEvent(JSON.parse(data) as ProgressEvent)
      } catch {
        // A malformed event is not worth tearing the stream down for: the
        // outcome is read back from the store when the stream ends, so a lost
        // progress line costs nothing.
      }
    }
  }
}
