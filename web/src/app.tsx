import { useCallback, useEffect, useState } from 'react'

import * as api from './api'
import type { Dataset } from './api'
import { Upload } from './components/upload'
import { DatasetScreen } from './screens/dataset'
import { RunScreen } from './screens/run'
import { match, onLinkClick, usePath } from './router'

export function App() {
  const path = usePath()
  const [datasets, setDatasets] = useState<Dataset[]>([])
  const [needsToken, setNeedsToken] = useState(false)
  const [error, setError] = useState('')

  const loadDatasets = useCallback(async () => {
    try {
      setDatasets(await api.listDatasets())
      setNeedsToken(false)
      setError('')
    } catch (e) {
      if (e instanceof api.Unauthorized) {
        setNeedsToken(true)
        return
      }
      setError(e instanceof Error ? e.message : String(e))
    }
  }, [])

  useEffect(() => {
    void loadDatasets()
  }, [loadDatasets])

  if (needsToken) return <TokenGate onAccepted={loadDatasets} />

  return (
    <div className="shell">
      <aside className="sidebar">
        <div className="wordmark">
          <span className="v">Veri</span>tix
        </div>

        <Upload onAdded={loadDatasets} />

        <div className="grow">
          <div className="section-label">Datasets</div>
          {datasets.length === 0 ? (
            <p className="sub">None yet.</p>
          ) : (
            <ul className="dataset-list">
              {datasets.map((d) => {
                const to = `/datasets/${d.id}`
                return (
                  <li key={d.id}>
                    <a
                      href={to}
                      className={path === to ? 'current' : ''}
                      onClick={onLinkClick(to)}
                      title={d.path}
                    >
                      {d.name}
                    </a>
                  </li>
                )
              })}
            </ul>
          )}
        </div>

        <Colophon />
      </aside>

      <main>
        {error && <p className="notice error">{error}</p>}
        <Route path={path} onDatasetsChanged={loadDatasets} />
      </main>
    </div>
  )
}

/*
The source-code offer, on every screen including the token gate.

AGPL section 13 asks a modified Veritix served over a network to offer its
users the source of the version they are using. The URL comes from the server
(`server.source_url`, reported by /health) rather than being baked into this
bundle, so an operator running a modified build points it at their own
repository without needing Node to rebuild the interface. An operator who sets
it empty gets no link, which is the right behaviour for a build shipped under
the commercial licence instead.

The link leaves the origin. That is not a hole in `connect-src 'self'`: it is a
navigation the user asks for, carrying no data, not a fetch this page makes.
*/
function Colophon() {
  const [info, setInfo] = useState<api.Health | null>(null)

  useEffect(() => {
    // A failure here is not worth a message. The footer is an offer, not a
    // control, and the screen behind it works without it.
    void api.health().then(setInfo, () => {})
  }, [])

  if (!info) return null

  return (
    <footer className="colophon">
      <span>Veritix {info.version}</span>
      {info.source_url && (
        <a href={info.source_url} target="_blank" rel="noopener noreferrer">
          Source
        </a>
      )}
    </footer>
  )
}

function Route({
  path,
  onDatasetsChanged,
}: {
  path: string
  onDatasetsChanged: () => void
}) {
  let m = match('/datasets/:id', path)
  if (m) return <DatasetScreen datasetId={m.id} onChanged={onDatasetsChanged} />

  m = match('/runs/:id', path)
  if (m) return <RunScreen runId={m.id} />

  m = match('/runs/:id/findings/:fid', path)
  if (m) return <RunScreen runId={m.id} findingId={m.fid} />

  if (path === '/') return <Welcome />
  return <NotFound path={path} />
}

function Welcome() {
  return (
    <div className="centre">
      <h1>Audit a dataset</h1>
      <p className="sub">
        Drop a folder of exports into the panel on the left. Veritix reads the
        files, works out what each column actually holds, and reports what looks
        wrong — including the problems that live between files rather than inside
        one.
      </p>
      <p className="sub">
        Nothing is uploaded anywhere. This is your server, and the data stays on it.
      </p>
    </div>
  )
}

function NotFound({ path }: { path: string }) {
  return (
    <div className="centre">
      <h1>Nothing here</h1>
      <p className="sub mono">{path}</p>
      <p>
        <a href="/" onClick={onLinkClick('/')}>
          Start again
        </a>
      </p>
    </div>
  )
}

/*
The server takes a bearer token only when it was started with one, which is
mandatory for any non-loopback bind. The token is asked for once and kept for
the tab; see api.ts for why sessionStorage and not localStorage.
*/
function TokenGate({ onAccepted }: { onAccepted: () => void }) {
  const [token, setToken] = useState('')

  function submit(e: React.FormEvent) {
    e.preventDefault()
    api.setAuthToken(token.trim())
    onAccepted()
  }

  return (
    <form className="centre" onSubmit={submit}>
      <h1>Token needed</h1>
      <p className="sub">
        This Veritix server was started with an access token. It is the token
        passed to <code>--auth-token</code>.
      </p>
      <p className="row centred gap-l">
        <input
          type="password"
          value={token}
          onChange={(e) => setToken(e.target.value)}
          placeholder="Access token"
          autoFocus
        />
        <button className="btn primary" type="submit" disabled={!token.trim()}>
          Continue
        </button>
      </p>
      <Colophon />
    </form>
  )
}
