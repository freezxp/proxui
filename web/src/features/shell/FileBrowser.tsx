import { useCallback, useEffect, useRef, useState } from 'react'
import { api, detailOf, requestBlob, uploadFile } from '@/api/client'
import { absoluteTime, bytes } from '@/lib/format'
import type { RemoteFile, RemoteListing } from '@/api/types'

/**
 * The SFTP panel (SSH-09).
 *
 * It runs over the same connection as the terminal beside it, so it needs no
 * credential of its own and inherits the session's ownership check: the server
 * refuses a session id that is not the caller's, which is what stops a session
 * id from being a bearer token for someone else's files (SSH-10).
 */

interface Props {
  sessionId: string
  /** Where to open — the account's home directory, resolved on the guest. */
  home: string
  onClose: () => void
  /** Announced to the operator; the page owns the notice area. */
  onNotice: (text: string) => void
}

interface Transfer {
  name: string
  fraction: number
}

/** Splits a path into the crumbs a breadcrumb bar renders. */
function crumbs(path: string): { label: string; path: string }[] {
  const parts = path.split('/').filter(Boolean)
  const out = [{ label: '/', path: '/' }]
  let walked = ''
  for (const part of parts) {
    walked += `/${part}`
    out.push({ label: part, path: walked })
  }
  return out
}

export function FileBrowser({ sessionId, home, onClose, onNotice }: Props) {
  const [path, setPath] = useState(home || '/')
  const [listing, setListing] = useState<RemoteListing | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [transfer, setTransfer] = useState<Transfer | null>(null)
  const [dragging, setDragging] = useState(false)
  const fileInput = useRef<HTMLInputElement>(null)

  const load = useCallback(
    async (target: string) => {
      setLoading(true)
      setError('')
      try {
        const result = await api.get<RemoteListing>(
          `/ssh-sessions/${sessionId}/files?path=${encodeURIComponent(target)}`,
        )
        setListing(result)
        // The server answers with the path it actually read, which is how the
        // browser learns where "" resolved to on the first load.
        setPath(result.path)
      } catch (err) {
        setError(detailOf(err, 'The directory could not be read.'))
      } finally {
        setLoading(false)
      }
    },
    [sessionId],
  )

  useEffect(() => {
    void load(home || '/')
  }, [load, home])

  const refresh = useCallback(() => void load(path), [load, path])

  const download = useCallback(
    async (entry: RemoteFile) => {
      onNotice(`Downloading ${entry.name}…`)
      try {
        const blob = await requestBlob(
          `/ssh-sessions/${sessionId}/files/content?path=${encodeURIComponent(entry.path)}`,
        )
        // An object URL and a synthetic click: the access token lives in
        // memory, so a plain link would arrive at the server unauthenticated.
        const url = URL.createObjectURL(blob)
        const link = document.createElement('a')
        link.href = url
        link.download = entry.name
        document.body.appendChild(link)
        link.click()
        document.body.removeChild(link)
        // Revoked on the next tick rather than immediately: Safari cancels a
        // download whose URL is released while the click is still in flight.
        window.setTimeout(() => URL.revokeObjectURL(url), 10000)
        onNotice(`Downloaded ${entry.name}.`)
      } catch (err) {
        onNotice(detailOf(err, `Could not download ${entry.name}.`))
      }
    },
    [sessionId, onNotice],
  )

  const upload = useCallback(
    async (files: FileList | File[]) => {
      for (const file of Array.from(files)) {
        setTransfer({ name: file.name, fraction: 0 })
        try {
          await uploadFile(
            `/ssh-sessions/${sessionId}/files/content?path=${encodeURIComponent(path)}&name=${encodeURIComponent(file.name)}`,
            file,
            (fraction) => setTransfer({ name: file.name, fraction }),
          )
          onNotice(`Uploaded ${file.name}.`)
        } catch (err) {
          onNotice(detailOf(err, `Could not upload ${file.name}.`))
        } finally {
          setTransfer(null)
        }
      }
      refresh()
    },
    [sessionId, path, onNotice, refresh],
  )

  // Every change to the guest's filesystem does the same three things: make the
  // call, re-read the directory, and say what went wrong if it did not work.
  const mutate = useCallback(
    async (call: Promise<unknown>, failure: string) => {
      try {
        await call
        refresh()
      } catch (err) {
        onNotice(detailOf(err, failure))
      }
    },
    [refresh, onNotice],
  )

  const mkdir = useCallback(() => {
    const name = window.prompt('Name for the new folder')
    if (!name) return
    void mutate(
      api.post(`/ssh-sessions/${sessionId}/files/mkdir`, { path, name }),
      'The folder could not be created.',
    )
  }, [sessionId, path, mutate])

  const rename = useCallback(
    (entry: RemoteFile) => {
      const next = window.prompt(`Rename or move ${entry.name}`, entry.path)
      if (!next || next === entry.path) return
      void mutate(
        api.post(`/ssh-sessions/${sessionId}/files/rename`, { path: entry.path, to: next }),
        'The rename failed.',
      )
    },
    [sessionId, mutate],
  )

  const chmod = useCallback(
    (entry: RemoteFile) => {
      const octal = (entry.mode_bits & 0o777).toString(8).padStart(3, '0')
      const next = window.prompt(`Permissions for ${entry.name}, in octal`, octal)
      if (!next || next === octal) return
      void mutate(
        api.post(`/ssh-sessions/${sessionId}/files/chmod`, { path: entry.path, mode: next }),
        'The mode could not be changed.',
      )
    },
    [sessionId, mutate],
  )

  const remove = useCallback(
    (entry: RemoteFile) => {
      // A directory has to be empty, which the guest enforces. Saying so up
      // front beats a "directory not empty" from the far end.
      const what = entry.is_dir ? 'folder (it must be empty)' : 'file'
      if (!window.confirm(`Delete the ${what} ${entry.path}?`)) return
      void mutate(
        api.del(`/ssh-sessions/${sessionId}/files?path=${encodeURIComponent(entry.path)}`),
        'The delete failed.',
      )
    },
    [sessionId, mutate],
  )

  return (
    <aside
      aria-label="Files"
      className="flex h-full w-full flex-col border-border bg-surface sm:w-[28rem] sm:shrink-0 sm:border-l"
      onDragOver={(event) => {
        event.preventDefault()
        setDragging(true)
      }}
      onDragLeave={() => setDragging(false)}
      onDrop={(event) => {
        event.preventDefault()
        setDragging(false)
        if (event.dataTransfer.files.length > 0) void upload(event.dataTransfer.files)
      }}
    >
      <header className="flex items-center justify-between gap-2 border-b border-border px-3 py-2">
        <h2 className="text-sm font-medium">Files</h2>
        <div className="flex items-center gap-1">
          <PanelButton onClick={() => fileInput.current?.click()}>Upload</PanelButton>
          <PanelButton onClick={mkdir}>New folder</PanelButton>
          <PanelButton onClick={refresh}>Refresh</PanelButton>
          <PanelButton onClick={onClose} aria-label="Close the file panel">
            ✕
          </PanelButton>
        </div>
      </header>

      <input
        ref={fileInput}
        type="file"
        multiple
        className="hidden"
        onChange={(event) => {
          if (event.target.files?.length) void upload(event.target.files)
          event.target.value = ''
        }}
      />

      <nav className="flex flex-wrap items-center gap-1 border-b border-border px-3 py-2 text-xs">
        {crumbs(path).map((crumb, index) => (
          <span key={crumb.path} className="flex items-center gap-1">
            {index > 0 && <span className="text-muted">/</span>}
            <button
              onClick={() => void load(crumb.path)}
              className="rounded px-1 py-0.5 font-mono hover:bg-surface-raised"
            >
              {crumb.label}
            </button>
          </span>
        ))}
      </nav>

      {transfer && (
        <div className="border-b border-border px-3 py-2 text-xs">
          <p className="truncate text-muted">Uploading {transfer.name}</p>
          <div className="mt-1 h-1 w-full overflow-hidden rounded bg-surface-raised">
            <div
              className="h-full bg-accent transition-[width]"
              style={{ width: `${Math.round(transfer.fraction * 100)}%` }}
            />
          </div>
        </div>
      )}

      <div className="relative flex-1 overflow-y-auto">
        {dragging && (
          <div className="pointer-events-none absolute inset-0 z-10 flex items-center justify-center border-2 border-dashed border-accent bg-surface/80 text-sm">
            Drop to upload into {path}
          </div>
        )}

        {loading && <p className="p-3 text-sm text-muted">Reading {path}…</p>}
        {!loading && error !== '' && <p className="p-3 text-sm text-danger">{error}</p>}
        {!loading && error === '' && listing && (
          <table className="w-full text-left text-xs">
            <thead className="sticky top-0 bg-surface text-muted">
              <tr>
                <th className="px-3 py-1 font-medium">Name</th>
                <th className="px-2 py-1 text-right font-medium">Size</th>
                <th className="px-2 py-1 font-medium">Mode</th>
                <th className="px-2 py-1 font-medium">Modified</th>
                <th className="px-2 py-1" />
              </tr>
            </thead>
            <tbody>
              {listing.parent !== '' && (
                <tr className="hover:bg-surface-raised">
                  <td colSpan={5} className="px-3 py-1">
                    <button onClick={() => void load(listing.parent)} className="font-mono">
                      ../
                    </button>
                  </td>
                </tr>
              )}
              {listing.data.length === 0 && (
                <tr>
                  <td colSpan={5} className="px-3 py-3 text-muted">
                    This folder is empty.
                  </td>
                </tr>
              )}
              {listing.data.map((entry) => (
                <tr key={entry.path} className="group hover:bg-surface-raised">
                  <td className="max-w-[14rem] truncate px-3 py-1">
                    <button
                      onClick={() => (entry.is_dir ? void load(entry.path) : void download(entry))}
                      className="font-mono"
                      title={entry.is_link ? `→ ${entry.target ?? ''}` : entry.path}
                    >
                      {entry.is_dir ? '📁' : '📄'} {entry.name}
                      {entry.is_link && <span className="text-muted"> ↗</span>}
                    </button>
                  </td>
                  <td className="px-2 py-1 text-right tabular-nums text-muted">
                    {entry.is_dir ? '—' : bytes(entry.size)}
                  </td>
                  <td className="px-2 py-1 font-mono text-muted">{entry.mode}</td>
                  <td className="whitespace-nowrap px-2 py-1 text-muted">
                    {absoluteTime(entry.mod_time)}
                  </td>
                  <td className="px-2 py-1">
                    {/* Shown on hover and on focus: keyboard users never
                        hover, and actions they cannot reach are not actions. */}
                    <span className="flex gap-1 opacity-0 transition-opacity focus-within:opacity-100 group-hover:opacity-100">
                      {!entry.is_dir && (
                        <RowButton onClick={() => void download(entry)}>Get</RowButton>
                      )}
                      <RowButton onClick={() => rename(entry)}>Rename</RowButton>
                      <RowButton onClick={() => chmod(entry)}>Mode</RowButton>
                      <RowButton onClick={() => remove(entry)}>Delete</RowButton>
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </aside>
  )
}

function PanelButton({
  children,
  onClick,
  ...rest
}: React.ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button
      onClick={onClick}
      className="rounded-md border border-border px-2 py-1 text-xs hover:bg-surface-raised"
      {...rest}
    >
      {children}
    </button>
  )
}

function RowButton({ children, onClick }: React.ButtonHTMLAttributes<HTMLButtonElement>) {
  return (
    <button onClick={onClick} className="rounded px-1 text-[11px] text-muted hover:text-content">
      {children}
    </button>
  )
}
