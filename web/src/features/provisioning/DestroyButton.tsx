import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { api } from '@/api/client'
import { Drawer } from '@/components/Drawer'

/** Destroy a guest (ADR 0010).
 *
 *  Typing the name is asked for here and checked again on the server, which is
 *  where the control actually lives: a confirmation a client could skip is not
 *  one. Until this feature the portal could not delete a guest at all, because
 *  its platform credential was not able to — this dialog and the admin-only
 *  route are what replaced that guarantee.
 */
export function DestroyButton({
  vmID,
  name,
  state,
}: {
  vmID: string
  name: string
  state: string
}) {
  const [open, setOpen] = useState(false)
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const queryClient = useQueryClient()
  const navigate = useNavigate()

  const destroy = useMutation({
    mutationFn: () =>
      api.del<{ request_id: string; state: string }>(`/vms/${vmID}`, { confirm_name: confirm }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['vms'] })
      setOpen(false)
      navigate('/vms')
    },
    onError: (err) =>
      setError(err instanceof Error ? err.message : 'The guest could not be destroyed.'),
  })

  const running = state === 'running'

  return (
    <>
      <button
        onClick={() => {
          setConfirm('')
          setError('')
          setOpen(true)
        }}
        className="rounded-md border border-state-error/40 px-3 py-2 text-sm font-medium text-state-error hover:bg-state-error/10"
      >
        Destroy
      </button>

      {open && (
        <Drawer
          title={`Destroy ${name}`}
          onClose={() => setOpen(false)}
          footer={
            <div className="flex items-center justify-between gap-3">
              {error && <span className="text-xs text-state-error">{error}</span>}
              <button
                onClick={() => destroy.mutate()}
                disabled={confirm.trim() !== name || destroy.isPending}
                className="ml-auto rounded-md bg-state-error px-3 py-1.5 text-sm text-white disabled:opacity-40"
              >
                {destroy.isPending ? 'Destroying…' : 'Destroy permanently'}
              </button>
            </div>
          }
        >
          <div className="space-y-4 text-sm">
            <p>
              This removes the guest and its disks from the platform. Nothing here can undo it, and
              the portal keeps no copy.
            </p>
            {running && (
              <p className="rounded-md bg-state-paused/10 p-3 text-xs text-state-paused">
                The guest is running. The platform will refuse to destroy it until it is stopped.
              </p>
            )}
            <label className="block">
              <span className="mb-1 block text-muted">
                Type <span className="font-mono text-fg">{name}</span> to confirm
              </span>
              <input
                value={confirm}
                onChange={(e) => setConfirm(e.target.value)}
                autoFocus
                className="w-full rounded-md border border-border bg-surface px-2 py-1.5 font-mono"
              />
            </label>
            <p className="text-xs text-muted">
              The guest disappears from the portal at the next synchronization, not immediately: the
              platform is the one that knows what it has.
            </p>
          </div>
        </Drawer>
      )}
    </>
  )
}
