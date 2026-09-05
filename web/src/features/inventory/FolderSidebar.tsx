import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { VMFolder } from '@/api/types'
import { useFolders } from './FolderPicker'

/** Which node of the folder pane is open. */
export type FolderSelection =
  { kind: 'all' } | { kind: 'favourites' } | { kind: 'unfiled' } | { kind: 'folder'; id: string }

/** The left pane of the folders view (INV-07).
 *
 *  A flat list rather than a tree, because folders are flat — there are no
 *  subfolders to unfold into, so the pane selects rather than expands, which is
 *  what a file manager's left pane amounts to once nesting is taken away.
 *
 *  It is also where a folder finally becomes editable: rename and delete have
 *  had working endpoints since folders shipped and nothing that called them.
 */
export function FolderSidebar({
  selection,
  onSelect,
  totalVMs,
  favouriteCount,
}: {
  selection: FolderSelection
  onSelect: (next: FolderSelection) => void
  /** From the list response the page already has, so the pane costs no extra
   *  request for the counts it cannot get from /folders. */
  totalVMs?: number
  favouriteCount?: number
}) {
  const folders = useFolders()
  const queryClient = useQueryClient()
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState('')
  const [renaming, setRenaming] = useState<string | null>(null)
  const [error, setError] = useState('')

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['folders'] })
    void queryClient.invalidateQueries({ queryKey: ['vms'] })
  }
  const fail = (fallback: string) => (err: unknown) =>
    setError(err instanceof Error ? err.message : fallback)

  const create = useMutation({
    mutationFn: () => api.post<VMFolder>('/folders', { name }),
    onSuccess: (folder) => {
      setCreating(false)
      setName('')
      refresh()
      onSelect({ kind: 'folder', id: folder.id })
    },
    onError: fail('Could not create the folder.'),
  })

  const rename = useMutation({
    mutationFn: ({ id, next, position }: { id: string; next: string; position: number }) =>
      api.patch(`/folders/${id}`, { name: next, position }),
    onSuccess: () => {
      setRenaming(null)
      refresh()
    },
    onError: fail('Could not rename the folder.'),
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/folders/${id}`),
    onSuccess: () => {
      refresh()
      onSelect({ kind: 'all' })
    },
    onError: fail('Could not delete the folder.'),
  })

  const list = folders.data?.data ?? []

  return (
    <nav aria-label="Folders" className="space-y-1 text-sm">
      <Node
        label="All VMs"
        count={totalVMs}
        active={selection.kind === 'all'}
        onClick={() => onSelect({ kind: 'all' })}
      />
      <Node
        label="★ Favourites"
        count={favouriteCount}
        active={selection.kind === 'favourites'}
        onClick={() => onSelect({ kind: 'favourites' })}
      />

      <hr className="my-2 border-border" />

      {list.map((folder) =>
        renaming === folder.id ? (
          <input
            key={folder.id}
            defaultValue={folder.name}
            autoFocus
            onBlur={() => setRenaming(null)}
            onKeyDown={(e) => {
              if (e.key === 'Escape') setRenaming(null)
              if (e.key === 'Enter') {
                const next = e.currentTarget.value.trim()
                if (next) rename.mutate({ id: folder.id, next, position: folder.position })
              }
            }}
            className="w-full rounded border border-border bg-surface px-2 py-1 text-sm"
          />
        ) : (
          <Node
            key={folder.id}
            label={folder.name}
            count={folder.vm_count}
            active={selection.kind === 'folder' && selection.id === folder.id}
            onClick={() => onSelect({ kind: 'folder', id: folder.id })}
            onRename={() => setRenaming(folder.id)}
            onDelete={() => remove.mutate(folder.id)}
            folderName={folder.name}
          />
        ),
      )}

      <Node
        label="Unfiled"
        active={selection.kind === 'unfiled'}
        onClick={() => onSelect({ kind: 'unfiled' })}
      />

      {creating ? (
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && name.trim()) create.mutate()
            if (e.key === 'Escape') setCreating(false)
          }}
          onBlur={() => !name.trim() && setCreating(false)}
          placeholder="Folder name"
          autoFocus
          className="mt-2 w-full rounded border border-border bg-surface px-2 py-1 text-sm"
        />
      ) : (
        <button
          onClick={() => setCreating(true)}
          className="mt-2 w-full rounded px-2 py-1 text-left text-xs text-muted hover:bg-surface-inset"
        >
          + New folder
        </button>
      )}

      {error && <p className="px-2 text-xs text-danger">{error}</p>}
    </nav>
  )
}

function Node({
  label,
  count,
  active,
  onClick,
  onRename,
  onDelete,
  folderName,
}: {
  label: string
  count?: number
  active: boolean
  onClick: () => void
  onRename?: () => void
  onDelete?: () => void
  folderName?: string
}) {
  const [confirming, setConfirming] = useState(false)

  return (
    <div
      className={`group flex items-center gap-1 rounded px-2 py-1 ${
        active ? 'bg-accent/10 text-accent' : 'hover:bg-surface-inset'
      }`}
    >
      <button onClick={onClick} aria-current={active} className="flex-1 text-left">
        {label}
        {count !== undefined && <span className="ml-1 text-xs text-muted">({count})</span>}
      </button>

      {onRename && !confirming && (
        <span className="hidden gap-1 group-hover:flex">
          <button
            onClick={onRename}
            title={`Rename ${folderName}`}
            aria-label={`Rename ${folderName}`}
            className="rounded px-1 text-xs text-muted hover:text-content"
          >
            ✎
          </button>
          <button
            onClick={() => setConfirming(true)}
            title={`Delete ${folderName}`}
            aria-label={`Delete ${folderName}`}
            className="rounded px-1 text-xs text-muted hover:text-danger"
          >
            ×
          </button>
        </span>
      )}

      {confirming && (
        <span className="flex items-center gap-1 text-xs">
          {/* Said plainly, because "delete folder" reads like it takes the
              machines with it, and it does not. */}
          <span className="text-muted">Delete? VMs stay.</span>
          <button
            onClick={() => {
              setConfirming(false)
              onDelete?.()
            }}
            className="rounded px-1 text-danger"
          >
            Yes
          </button>
          <button onClick={() => setConfirming(false)} className="rounded px-1 text-muted">
            No
          </button>
        </span>
      )}
    </div>
  )
}
