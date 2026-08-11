import { useRef, useState } from 'react'

import * as api from '../api'
import { navigate } from '../router'

/*
Supplying data is the first thing a user does, so it lives in the sidebar rather
than behind a screen.

A directory is one dataset, not one dataset per file — most real defects live in
the relationships between a folder of exports — so the drop zone gathers a whole
folder and sends it as a unit, and names the dataset after the folder.
*/
export function Upload({ onAdded }: { onAdded: () => void }) {
  const [over, setOver] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [showPath, setShowPath] = useState(false)
  const [path, setPath] = useState('')
  const picker = useRef<HTMLInputElement>(null)

  async function accept(name: string, files: File[]) {
    if (files.length === 0) {
      setError('That had no files in it.')
      return
    }
    setBusy(true)
    setError('')
    try {
      const ds = await api.uploadDataset(name, files)
      onAdded()
      navigate(`/datasets/${ds.id}`)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  async function onDrop(e: React.DragEvent) {
    e.preventDefault()
    setOver(false)
    if (busy) return

    const { name, files } = await gather(e.dataTransfer)
    void accept(name, files)
  }

  function onPicked(e: React.ChangeEvent<HTMLInputElement>) {
    const files = Array.from(e.target.files ?? [])
    e.target.value = '' // so picking the same folder twice fires again
    if (files.length === 0) return

    // A folder pick carries relative paths; the first segment is the folder the
    // user chose, and that is the dataset's name.
    const rel = (files[0] as FileWithPath).webkitRelativePath
    const name = rel ? rel.split('/')[0] : baseName(files[0].name)
    void accept(name, files)
  }

  async function register() {
    if (!path.trim()) return
    setBusy(true)
    setError('')
    try {
      const ds = await api.registerDataset(path.trim())
      onAdded()
      setPath('')
      setShowPath(false)
      navigate(`/datasets/${ds.id}`)
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div>
      <div className="section-label">Add data</div>

      <button
        type="button"
        className={`dropzone${over ? ' over' : ''}${busy ? ' busy' : ''}`}
        onClick={() => picker.current?.click()}
        onDragOver={(e) => {
          e.preventDefault()
          setOver(true)
        }}
        onDragLeave={() => setOver(false)}
        onDrop={onDrop}
        disabled={busy}
      >
        {busy ? 'Uploading…' : <>Drop a folder here<br />or choose one</>}
      </button>

      <input
        ref={picker}
        type="file"
        multiple
        hidden
        onChange={onPicked}
        // webkitdirectory is how a browser offers a folder picker. It is not in
        // the React DOM typings, hence the cast.
        {...({ webkitdirectory: '' } as Record<string, string>)}
      />

      {error && <p className="notice error">{error}</p>}

      {showPath ? (
        <div className="gap-s">
          <input
            type="text"
            value={path}
            placeholder="/srv/exports/retail"
            onChange={(e) => setPath(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && void register()}
            className="fill"
          />
          <div className="row gap-s">
            <button className="btn" onClick={register} disabled={busy}>
              Read it
            </button>
            <button className="btn link" onClick={() => setShowPath(false)}>
              Cancel
            </button>
          </div>
        </div>
      ) : (
        <button
          className="btn link gap-s small"
          onClick={() => setShowPath(true)}
        >
          or read a folder already on the server
        </button>
      )}
    </div>
  )
}

interface FileWithPath extends File {
  webkitRelativePath: string
}

function baseName(name: string): string {
  const dot = name.lastIndexOf('.')
  return dot > 0 ? name.slice(0, dot) : name
}

/**
 * gather pulls every file out of a drop, walking into directories.
 *
 * Dropping a folder yields a directory entry rather than its contents, so it has
 * to be walked. The server takes base names only and re-checks them, so a
 * relative path in a filename cannot escape the upload directory.
 */
async function gather(dt: DataTransfer): Promise<{ name: string; files: File[] }> {
  const entries: FileSystemEntry[] = []
  for (const item of Array.from(dt.items)) {
    const entry = item.webkitGetAsEntry?.()
    if (entry) entries.push(entry)
  }

  if (entries.length === 0) {
    // A browser that will not describe the drop as entries still gives the
    // flat file list, which is enough for a drop of loose files.
    const files = Array.from(dt.files)
    return { name: files.length === 1 ? baseName(files[0].name) : 'upload', files }
  }

  const files: File[] = []
  for (const entry of entries) await walk(entry, files)

  const soleDir = entries.length === 1 && entries[0].isDirectory
  const name = soleDir
    ? entries[0].name
    : files.length === 1
      ? baseName(files[0].name)
      : 'upload'
  return { name, files }
}

async function walk(entry: FileSystemEntry, out: File[]): Promise<void> {
  if (entry.isFile) {
    const file = await new Promise<File | null>((resolve) => {
      ;(entry as FileSystemFileEntry).file(resolve, () => resolve(null))
    })
    if (file) out.push(file)
    return
  }
  if (!entry.isDirectory) return

  const reader = (entry as FileSystemDirectoryEntry).createReader()
  for (;;) {
    // readEntries returns at most a batch at a time and signals the end with an
    // empty batch, so it has to be called until it does.
    const batch = await new Promise<FileSystemEntry[]>((resolve) => {
      reader.readEntries(resolve, () => resolve([]))
    })
    if (batch.length === 0) return
    for (const child of batch) await walk(child, out)
  }
}
