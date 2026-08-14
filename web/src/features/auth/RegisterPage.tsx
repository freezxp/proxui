import { useState } from 'react'
import { ApiError } from '@/api/client'
import { useAuth } from './useAuth'
import { useBranding } from '@/lib/branding'

const MIN_PASSWORD = 12

/** Creating an account gets you in, and gets you nothing else: a new account
 *  is read-only with no grants, so the inventory is empty until an
 *  administrator grants it something. The page says so rather than letting
 *  someone sign in and conclude the portal is broken. */
export function RegisterPage({ onDone, onCancel }: { onDone: () => void; onCancel: () => void }) {
  const branding = useBranding()
  const { registerAccount } = useAuth()

  const [username, setUsername] = useState('')
  const [email, setEmail] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [confirm, setConfirm] = useState('')
  const [error, setError] = useState('')
  const [fields, setFields] = useState<Record<string, string>>({})
  const [busy, setBusy] = useState(false)

  const tooShort = password.length > 0 && password.length < MIN_PASSWORD
  const mismatch = confirm.length > 0 && password !== confirm
  const ready =
    username.trim() && email.trim() && password.length >= MIN_PASSWORD && password === confirm

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError('')
    setFields({})
    try {
      await registerAccount({ username, email, display_name: displayName, password })
      onDone()
    } catch (err) {
      if (err instanceof ApiError) {
        setError(err.detail || err.message)
        setFields(err.fields ?? {})
      } else {
        setError('Could not create the account.')
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-full items-center justify-center px-4 py-8">
      <form
        onSubmit={onSubmit}
        className="w-full max-w-sm space-y-5 rounded-lg border border-border bg-surface-raised p-8 shadow-sm"
      >
        <div>
          <div className="flex items-center gap-3">
            {branding['branding.logo'] && (
              <img
                src={branding['branding.logo']}
                alt=""
                aria-hidden="true"
                className="h-9 w-auto"
              />
            )}
            <h1 className="text-xl font-semibold">{branding['branding.portal_name']}</h1>
          </div>
          <p className="mt-1 text-sm text-muted">Create an account</p>
        </div>

        <p className="rounded-md border border-border bg-surface p-3 text-xs text-muted">
          New accounts can sign in but see nothing until an administrator grants them access to
          machines.
        </p>

        <Field
          label="Username"
          value={username}
          onChange={setUsername}
          autoFocus
          error={fields.username && 'Lowercase letters, digits, dot, dash or underscore.'}
          help="3 to 32 characters."
        />
        <Field
          label="Email"
          value={email}
          onChange={setEmail}
          type="email"
          error={fields.email && 'That is not an email address.'}
        />
        <Field
          label="Display name"
          value={displayName}
          onChange={setDisplayName}
          help="Optional. How your name appears to others."
        />
        <Field
          label="Password"
          value={password}
          onChange={setPassword}
          type="password"
          error={tooShort ? `At least ${MIN_PASSWORD} characters.` : fields.password && error}
          help={`At least ${MIN_PASSWORD} characters. Length matters more than punctuation.`}
        />
        <Field
          label="Confirm password"
          value={confirm}
          onChange={setConfirm}
          type="password"
          error={mismatch ? 'The two entries do not match.' : undefined}
        />

        {error && !fields.username && !fields.email && !fields.password && (
          <p className="text-sm text-danger">{error}</p>
        )}

        <button
          type="submit"
          disabled={!ready || busy}
          className="w-full rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
        >
          {busy ? 'Creating…' : 'Create account'}
        </button>

        <button
          type="button"
          onClick={onCancel}
          className="w-full text-center text-sm text-muted hover:text-content"
        >
          Back to sign in
        </button>
      </form>
    </div>
  )
}

function Field({
  label,
  value,
  onChange,
  type = 'text',
  error,
  help,
  autoFocus,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  type?: string
  error?: string | false
  help?: string
  autoFocus?: boolean
}) {
  const id = `reg-${label.replace(/\W+/g, '-').toLowerCase()}`
  return (
    <label className="block space-y-1.5" htmlFor={id}>
      <span className="text-sm font-medium">{label}</span>
      <input
        id={id}
        type={type}
        value={value}
        autoFocus={autoFocus}
        autoComplete={type === 'password' ? 'new-password' : 'off'}
        onChange={(e) => onChange(e.target.value)}
        className={`w-full rounded-md border bg-surface px-3 py-2 text-sm outline-none focus:ring-1 focus:ring-accent ${
          error ? 'border-danger' : 'border-border focus:border-accent'
        }`}
      />
      {error ? (
        <span className="block text-xs text-danger">{error}</span>
      ) : (
        help && <span className="block text-xs text-muted">{help}</span>
      )}
    </label>
  )
}
