import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { api, ApiError } from '@/api/client'

const MIN_LENGTH = 12

/** Changing a password ends every session, including this one. The dialog says
 *  so before the change rather than after, because being signed out
 *  unexpectedly reads as a bug. */
export function ChangePasswordDialog({
  forced,
  onClose,
  onChanged,
}: {
  /** A forced change cannot be dismissed: the account is unusable until it
   *  happens, so offering Cancel would only lead back to this dialog. */
  forced?: boolean
  onClose?: () => void
  onChanged: () => void
}) {
  const [current, setCurrent] = useState('')
  const [next, setNext] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [fieldErrors, setFieldErrors] = useState<Record<string, string>>({})

  const change = useMutation({
    mutationFn: () => api.post('/auth/password', { current_password: current, new_password: next }),
    onSuccess: onChanged,
    onError: (err) => {
      if (err instanceof ApiError) {
        setError(err.detail || err.message)
        setFieldErrors(err.fields ?? {})
        return
      }
      setError('Could not change the password.')
    },
  })

  const tooShort = next.length > 0 && next.length < MIN_LENGTH
  const mismatch = confirm.length > 0 && next !== confirm
  const same = next.length > 0 && next === current
  const ready = current && next.length >= MIN_LENGTH && next === confirm && !same

  const form = (
    <div className="space-y-4">
      {forced ? (
        <p className="rounded-md bg-paused/10 p-3 text-sm text-paused">
          This account was given a temporary password. Choose your own before continuing.
        </p>
      ) : (
        <p className="text-sm text-muted">
          You will be signed out everywhere, on this device and any other.
        </p>
      )}

      <Field
        label="Current password"
        value={current}
        onChange={setCurrent}
        error={fieldErrors.current_password && 'That is not your current password.'}
        autoFocus
      />
      <Field
        label="New password"
        value={next}
        onChange={setNext}
        error={
          tooShort
            ? `At least ${MIN_LENGTH} characters.`
            : same
              ? 'Must differ from your current password.'
              : fieldErrors.new_password && (error || 'Not accepted.')
        }
        help={`At least ${MIN_LENGTH} characters. Length matters more than punctuation.`}
      />
      <Field
        label="Confirm new password"
        value={confirm}
        onChange={setConfirm}
        error={mismatch ? 'The two entries do not match.' : undefined}
      />

      {error && !fieldErrors.new_password && !fieldErrors.current_password && (
        <p className="text-sm text-danger">{error}</p>
      )}

      <div className="flex justify-end gap-2 pt-1">
        {!forced && onClose && (
          <button onClick={onClose} className="rounded-md border border-border px-3 py-2 text-sm">
            Cancel
          </button>
        )}
        <button
          onClick={() => change.mutate()}
          disabled={!ready || change.isPending}
          className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
        >
          {change.isPending ? 'Changing…' : 'Change password'}
        </button>
      </div>
    </div>
  )

  // A forced change owns the screen; a voluntary one is a dialog over it.
  if (forced) {
    return (
      <div className="flex min-h-full items-center justify-center p-6">
        <div className="w-full max-w-md space-y-4 rounded-lg border border-border bg-surface p-6">
          <h1 className="text-lg font-semibold">Choose a password</h1>
          {form}
        </div>
      </div>
    )
  }

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-6">
      <div className="absolute inset-0 bg-black/40" onClick={onClose} aria-hidden="true" />
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Change password"
        className="relative w-full max-w-md space-y-4 rounded-lg border border-border bg-surface p-6 shadow-xl"
      >
        <h2 className="font-medium">Change password</h2>
        {form}
      </div>
    </div>
  )
}

function Field({
  label,
  value,
  onChange,
  error,
  help,
  autoFocus,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  error?: string | false
  help?: string
  autoFocus?: boolean
}) {
  const id = `pw-${label.replace(/\W+/g, '-').toLowerCase()}`
  return (
    <div className="space-y-1">
      <label htmlFor={id} className="block text-sm">
        {label}
      </label>
      <input
        id={id}
        type="password"
        value={value}
        autoFocus={autoFocus}
        autoComplete={label.startsWith('Current') ? 'current-password' : 'new-password'}
        onChange={(e) => onChange(e.target.value)}
        className={`w-full rounded-md border bg-surface px-3 py-2 text-sm ${
          error ? 'border-danger' : 'border-border'
        }`}
      />
      {error ? (
        <p className="text-xs text-danger">{error}</p>
      ) : (
        help && <p className="text-xs text-muted">{help}</p>
      )}
    </div>
  )
}
