import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { api, ApiError } from '@/api/client'
import type { VMState } from '@/api/types'

export type PowerAction = 'start' | 'stop' | 'shutdown' | 'reboot'

interface Choice {
  action: PowerAction
  label: string
  primary?: boolean
  danger?: boolean
  /** Absent means the action goes through on the first click. */
  confirm?: { title: string; body: string; verb: string }
}

const START: Choice = { action: 'start', label: 'Start', primary: true }

const SHUTDOWN: Choice = {
  action: 'shutdown',
  label: 'Shut down',
  confirm: {
    title: 'Shut down this VM?',
    body: 'The guest is asked to shut down cleanly. Anything running on it stops.',
    verb: 'Shut down',
  },
}

const REBOOT: Choice = {
  action: 'reboot',
  label: 'Reboot',
  confirm: {
    title: 'Reboot this VM?',
    body: 'The guest is asked to restart. It is unreachable until it comes back.',
    verb: 'Reboot',
  },
}

// Proxmox `stop` kills the guest immediately — the equivalent of pulling the
// power lead, with the filesystem damage that implies. The wording says so,
// and it is styled apart from the graceful pair so the two are not one row of
// interchangeable buttons.
const STOP: Choice = {
  action: 'stop',
  label: 'Force stop',
  danger: true,
  confirm: {
    title: 'Force stop this VM?',
    body:
      'The VM is cut off immediately, without telling the guest. Unsaved work is lost ' +
      'and filesystems can be left dirty. Prefer Shut down unless the guest is unresponsive.',
    verb: 'Force stop',
  },
}

/**
 * What may be done to a VM in a given state.
 *
 * An unknown state is one the portal has not managed to read, so it offers
 * both directions and lets the platform be the authority on which makes sense.
 */
function choicesFor(state: VMState): Choice[] {
  switch (state) {
    case 'running':
      return [SHUTDOWN, REBOOT, STOP]
    case 'stopped':
      return [START]
    case 'paused':
    case 'suspended':
      return [START, STOP]
    default:
      return [START, STOP]
  }
}

/**
 * Start, shut down, reboot and force stop.
 *
 * The platform answers 202: it has accepted a task, not finished it. So a
 * success here reports that the request was taken, and the caller is told to
 * watch for the state to change rather than being shown a state that has not
 * happened yet.
 */
export function PowerControls({
  vmId,
  state,
  onRequested,
}: {
  vmId: string
  state: VMState
  /** Called once the platform has accepted the task. */
  onRequested: (action: PowerAction) => void
}) {
  const [confirming, setConfirming] = useState<Choice | null>(null)
  const [error, setError] = useState('')

  const power = useMutation({
    mutationFn: (action: PowerAction) => api.post(`/vms/${vmId}/power`, { action }),
    onSuccess: (_data, action) => {
      setError('')
      setConfirming(null)
      onRequested(action)
    },
    onError: (err) => {
      setConfirming(null)
      setError(powerError(err))
    },
  })

  const run = (choice: Choice) => {
    if (choice.confirm) setConfirming(choice)
    else power.mutate(choice.action)
  }

  return (
    <div className="flex flex-col items-end gap-2">
      <div className="flex flex-wrap items-center gap-2">
        {choicesFor(state).map((choice) => (
          <button
            key={choice.action}
            onClick={() => run(choice)}
            disabled={power.isPending}
            className={`rounded-md px-3 py-2 text-sm font-medium disabled:opacity-40 ${
              choice.primary
                ? 'bg-accent text-white'
                : choice.danger
                  ? 'border border-danger text-danger hover:bg-danger/10'
                  : 'border border-border hover:bg-surface-raised'
            }`}
          >
            {choice.label}
          </button>
        ))}
      </div>

      {error && (
        <p role="alert" className="max-w-xs text-right text-xs text-danger">
          {error}
        </p>
      )}

      {confirming && (
        <ConfirmDialog
          choice={confirming}
          pending={power.isPending}
          onCancel={() => setConfirming(null)}
          onConfirm={() => power.mutate(confirming.action)}
        />
      )}
    </div>
  )
}

function ConfirmDialog({
  choice,
  pending,
  onCancel,
  onConfirm,
}: {
  choice: Choice
  pending: boolean
  onCancel: () => void
  onConfirm: () => void
}) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-6">
      <div className="absolute inset-0 bg-black/40" onClick={onCancel} aria-hidden="true" />
      <div
        role="dialog"
        aria-modal="true"
        aria-label={choice.confirm?.title}
        className="relative w-full max-w-md space-y-4 rounded-lg border border-border bg-surface-raised p-6 text-left"
      >
        <h2 className="text-lg font-semibold">{choice.confirm?.title}</h2>
        <p className="text-sm text-muted">{choice.confirm?.body}</p>
        <div className="flex justify-end gap-2">
          <button
            onClick={onCancel}
            className="rounded-md border border-border px-3 py-2 text-sm hover:bg-surface"
          >
            Cancel
          </button>
          <button
            onClick={onConfirm}
            disabled={pending}
            autoFocus
            className={`rounded-md px-3 py-2 text-sm font-medium text-white disabled:opacity-40 ${
              choice.danger ? 'bg-danger' : 'bg-accent'
            }`}
          >
            {pending ? 'Working…' : choice.confirm?.verb}
          </button>
        </div>
      </div>
    </div>
  )
}

// The causes an operator can act on are named. A power action reaching the
// platform and being refused there is a different problem from one the portal
// refused, and they need different people to fix them.
function powerError(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 404) return 'This VM is no longer visible to your account.'
    if (err.status === 429) return 'Too many actions. Wait a moment and try again.'
    if (err.code === 'platform.permission_denied')
      return 'The platform credential is not allowed to perform power actions.'
    if (err.code === 'platform.unreachable') return 'The platform could not be reached.'
    return err.detail || err.message
  }
  return 'The power action could not be sent.'
}
