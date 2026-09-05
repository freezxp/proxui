import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { VMFolder } from '@/api/types'

/** The caller's own folders. Shared by the picker, the filter and the bulk
 *  action, so all three agree about what exists without three requests. */
export function useFolders() {
  return useQuery({
    queryKey: ['folders'],
    queryFn: () => api.get<{ data: VMFolder[] }>('/folders'),
    staleTime: 30_000,
  })
}

/** File a VM into one of your folders (INV-17).
 *
 *  A select rather than drag-and-drop: it works with a keyboard, works on a
 *  touchscreen, and does not have to care that the folder you are aiming at
 *  might be on another page of a paginated list.
 *
 *  Creating a folder is offered inline, because otherwise filing the first VM
 *  means leaving for a settings screen and coming back.
 */
export function FolderPicker({
  vmID,
  folderID,
  invalidate = ['vms'],
}: {
  vmID: string
  folderID?: string | null
  invalidate?: string[]
}) {
  const queryClient = useQueryClient()
  const folders = useFolders()
  const [creating, setCreating] = useState(false)
  const [name, setName] = useState('')
  const [error, setError] = useState('')

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: invalidate })
    void queryClient.invalidateQueries({ queryKey: ['folders'] })
  }

  const file = useMutation({
    mutationFn: (target: string | null) => api.put(`/vms/${vmID}/folder`, { folder_id: target }),
    onSuccess: refresh,
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not file this VM.'),
  })

  const create = useMutation({
    mutationFn: async () => {
      const folder = await api.post<VMFolder>('/folders', { name })
      await api.put(`/vms/${vmID}/folder`, { folder_id: folder.id })
      return folder
    },
    onSuccess: () => {
      setCreating(false)
      setName('')
      refresh()
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not create the folder.'),
  })

  if (creating) {
    return (
      <span className="flex items-center gap-1">
        <input
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' && name.trim()) create.mutate()
            if (e.key === 'Escape') setCreating(false)
          }}
          placeholder="Folder name"
          autoFocus
          className="w-32 rounded border border-border bg-surface px-1.5 py-0.5 text-xs"
        />
        <button
          onClick={() => name.trim() && create.mutate()}
          disabled={!name.trim() || create.isPending}
          className="rounded border border-border px-1.5 py-0.5 text-xs disabled:opacity-40"
        >
          Add
        </button>
      </span>
    )
  }

  return (
    <span title={error || undefined}>
      <select
        value={folderID ?? ''}
        disabled={file.isPending}
        onChange={(e) => {
          setError('')
          if (e.target.value === '__new__') {
            setCreating(true)
            return
          }
          file.mutate(e.target.value || null)
        }}
        className={`w-full max-w-[10rem] rounded border border-border bg-surface px-1.5 py-0.5 text-xs ${
          error ? 'border-state-error' : ''
        }`}
      >
        <option value="">Unfiled</option>
        {(folders.data?.data ?? []).map((f) => (
          <option key={f.id} value={f.id}>
            {f.name}
          </option>
        ))}
        <option value="__new__">New folder…</option>
      </select>
    </span>
  )
}
