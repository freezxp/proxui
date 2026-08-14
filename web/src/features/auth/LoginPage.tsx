import { useState } from 'react'
import { useAuth } from './useAuth'
import { useBranding } from '@/lib/branding'
import { ApiError } from '@/api/client'

export function LoginPage() {
  const branding = useBranding()
  const { login } = useAuth()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError(null)
    try {
      await login(username, password)
    } catch (err) {
      // The server deliberately does not say which half was wrong; repeating
      // its message keeps the UI from inventing a distinction that would help
      // an attacker enumerate accounts.
      if (err instanceof ApiError) {
        setError(
          err.status === 423
            ? 'This account is temporarily locked after too many failed attempts.'
            : err.status === 429
              ? 'Too many attempts. Wait a moment and try again.'
              : 'Invalid credentials.',
        )
      } else {
        setError('Could not reach the portal.')
      }
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex min-h-full items-center justify-center px-4">
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
          <p className="mt-1 text-sm text-muted">Sign in to continue</p>
        </div>

        {branding['branding.login_banner'] && (
          <p className="rounded-md border border-border bg-surface p-3 text-xs text-muted">
            {branding['branding.login_banner']}
          </p>
        )}

        <label className="block space-y-1.5">
          <span className="text-sm font-medium">Username</span>
          <input
            autoFocus
            autoComplete="username"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm outline-none focus:border-accent focus:ring-1 focus:ring-accent"
          />
        </label>

        <label className="block space-y-1.5">
          <span className="text-sm font-medium">Password</span>
          <input
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm outline-none focus:border-accent focus:ring-1 focus:ring-accent"
          />
        </label>

        {error && (
          <p role="alert" className="rounded-md bg-danger/10 px-3 py-2 text-sm text-danger">
            {error}
          </p>
        )}

        <button
          type="submit"
          disabled={busy || !username || !password}
          className="w-full rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-50"
        >
          {busy ? 'Signing in…' : 'Sign in'}
        </button>
      </form>
    </div>
  )
}
