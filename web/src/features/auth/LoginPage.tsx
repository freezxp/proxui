import { useState } from 'react'
import { useAuth } from './useAuth'
import { useBranding } from '@/lib/branding'
import { useAuthMethods, SSO_MESSAGES } from '@/lib/authMethods'
import { ApiError } from '@/api/client'

export function LoginPage({ onRegister }: { onRegister: () => void }) {
  const branding = useBranding()
  const methods = useAuthMethods()
  // The Google callback redirects here with a reason when it could not
  // finish, rather than rendering a page of its own.
  const ssoError = new URLSearchParams(window.location.search).get('sso')
  const { login, verifyMFA } = useAuth()
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState<string | null>(null)
  const [busy, setBusy] = useState(false)
  // Set once the password has been accepted and a code is owed (AUTH-04).
  // Holding it here rather than routing to a second page keeps the half-signed
  // -in state in one component, where it dies with a reload as it should.
  const [mfaToken, setMfaToken] = useState<string | null>(null)
  const [code, setCode] = useState('')

  async function onSubmit(event: React.FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError(null)
    try {
      const challenge = await login(username, password)
      if (challenge) {
        setMfaToken(challenge.mfa_token)
        setCode('')
        return
      }
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

  async function onVerify(event: React.FormEvent) {
    event.preventDefault()
    if (!mfaToken) return
    setBusy(true)
    setError(null)
    try {
      await verifyMFA(mfaToken, code)
    } catch (err) {
      if (err instanceof ApiError && err.code === 'auth.mfa_challenge_expired') {
        // The challenge is gone — too many wrong codes, or too long spent
        // looking for the phone. There is nothing to retry against, so the
        // form goes back to the password rather than to a prompt that will
        // never accept anything again.
        setMfaToken(null)
        setPassword('')
        setError('That sign-in attempt expired. Sign in again.')
      } else if (err instanceof ApiError && err.status === 429) {
        setError('Too many attempts. Wait a moment and try again.')
      } else if (err instanceof ApiError) {
        setError('That code is not valid. Check your authenticator app and try again.')
      } else {
        setError('Could not reach the portal.')
      }
      setCode('')
    } finally {
      setBusy(false)
    }
  }

  if (mfaToken) {
    return (
      <div className="flex min-h-full items-center justify-center px-4">
        <form
          onSubmit={onVerify}
          className="w-full max-w-sm space-y-5 rounded-lg border border-border bg-surface-raised p-8 shadow-sm"
        >
          <div>
            <h1 className="text-xl font-semibold">Two-step verification</h1>
            <p className="mt-1 text-sm text-muted">
              Enter the 6-digit code from your authenticator app.
            </p>
          </div>

          <label className="block space-y-1.5">
            <span className="text-sm font-medium">Code</span>
            <input
              autoFocus
              // one-time-code lets a phone offer the code from its keyboard,
              // which is most of what makes this bearable on mobile.
              autoComplete="one-time-code"
              inputMode="numeric"
              pattern="[0-9]*"
              maxLength={6}
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))}
              className="w-full rounded-md border border-border bg-surface px-3 py-2 text-center font-mono text-lg tracking-[0.4em] outline-none focus:border-accent focus:ring-1 focus:ring-accent"
            />
          </label>

          {error && (
            <p role="alert" className="rounded-md bg-danger/10 px-3 py-2 text-sm text-danger">
              {error}
            </p>
          )}

          <button
            type="submit"
            disabled={busy || code.length !== 6}
            className="w-full rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-50"
          >
            {busy ? 'Verifying…' : 'Verify'}
          </button>

          <button
            type="button"
            onClick={() => {
              setMfaToken(null)
              setPassword('')
              setError(null)
            }}
            className="w-full text-center text-sm text-muted hover:underline"
          >
            Cancel
          </button>
        </form>
      </div>
    )
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

        {ssoError && (
          <p className="rounded-md border border-danger/40 bg-danger/5 p-3 text-sm text-danger">
            {SSO_MESSAGES[ssoError] ?? 'That sign-in could not be completed.'}
          </p>
        )}

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

        {methods.google && (
          <>
            <div className="flex items-center gap-3 text-xs text-muted">
              <span className="h-px flex-1 bg-border" />
              or
              <span className="h-px flex-1 bg-border" />
            </div>
            {/* A plain link, not a fetch: the browser has to actually navigate
                to Google, and the callback comes back as a navigation too. */}
            <a
              href="/api/v1/auth/google/start"
              className="flex w-full items-center justify-center gap-2 rounded-md border border-border px-3 py-2 text-sm hover:bg-surface-inset"
            >
              <GoogleMark />
              Sign in with Google
            </a>
          </>
        )}

        {methods.registration && (
          <p className="text-center text-sm text-muted">
            No account?{' '}
            <button type="button" onClick={onRegister} className="text-accent hover:underline">
              Create one
            </button>
          </p>
        )}
      </form>
    </div>
  )
}

/** Google's mark, inline so the sign-in page reaches no other origin — the
 *  content security policy would block it, and a button with a missing image
 *  looks broken. */
function GoogleMark() {
  return (
    <svg width="16" height="16" viewBox="0 0 48 48" aria-hidden="true">
      <path
        fill="#4285F4"
        d="M45 24c0-1.6-.1-2.7-.4-4H24v7.5h12c-.2 2-1.5 5-4.4 7l6.7 5.2C42.2 36 45 30.6 45 24z"
      />
      <path
        fill="#34A853"
        d="M24 46c5.9 0 10.9-2 14.5-5.3l-6.9-5.4c-1.9 1.3-4.4 2.2-7.6 2.2-5.8 0-10.7-3.9-12.5-9.2l-7.1 5.5C8.1 41 15.4 46 24 46z"
      />
      <path
        fill="#FBBC05"
        d="M11.5 28.3A13.6 13.6 0 0 1 10.8 24c0-1.5.3-3 .7-4.3l-7.1-5.5A22 22 0 0 0 2 24c0 3.6.9 7 2.4 9.8l7.1-5.5z"
      />
      <path
        fill="#EA4335"
        d="M24 10.4c3.2 0 6 1.1 8.2 3.2l6.1-6.1C34.9 4 29.9 2 24 2 15.4 2 8.1 7 4.4 14.2l7.1 5.5C13.3 14.3 18.2 10.4 24 10.4z"
      />
    </svg>
  )
}
