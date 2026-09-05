import { useState } from 'react'
import QRCode from 'qrcode'
import { api, detailOf } from '@/api/client'
import type { TOTPEnrollment } from '@/api/types'
import { useAuth } from './useAuth'
import { copyText } from '@/lib/clipboard'

/**
 * Enrolling and removing this account's own second factor (AUTH-04).
 *
 * Three things this deliberately does. The QR code is drawn in the browser
 * from the otpauth URL, so the seed never travels to an image service. The
 * secret is also shown as text, because a QR code is useless on the machine
 * that is also the phone, and because some authenticator apps only take typed
 * input. And enrolment is not finished until a code is entered — until then
 * the factor is inert, so a badly scanned QR is a retry rather than a locked
 * account somebody has to call an administrator about.
 */
export function TwoFactorPanel() {
  const { user, reload } = useAuth()
  const [enrollment, setEnrollment] = useState<TOTPEnrollment | null>(null)
  const [qr, setQR] = useState<string>('')
  const [code, setCode] = useState('')
  const [password, setPassword] = useState('')
  const [removing, setRemoving] = useState(false)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [busy, setBusy] = useState(false)
  const [copied, setCopied] = useState(false)

  const fail = (err: unknown, fallback: string) => setError(detailOf(err, fallback))

  async function begin() {
    setBusy(true)
    setError('')
    setNotice('')
    try {
      const started = await api.post<TOTPEnrollment>('/auth/me/totp', {})
      setEnrollment(started)
      setCode('')
      // Rendered to a data URI: the page reaches no other origin, which is
      // also what the content security policy allows. Done here rather than in
      // an effect because this click is the only thing that ever produces an
      // enrolment — a stale QR is unreachable, since it renders only inside
      // the branch this same call opens.
      //
      // A missing QR is survivable: the secret is right there to type.
      setQR(await QRCode.toDataURL(started.otpauth_url, { margin: 1, width: 200 }).catch(() => ''))
    } catch (err) {
      fail(err, 'Enrolment could not be started.')
    } finally {
      setBusy(false)
    }
  }

  async function confirm(event: React.FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError('')
    try {
      await api.post('/auth/me/totp/confirm', { code })
      setEnrollment(null)
      setQR('')
      setCode('')
      setNotice('Two-step verification is on. You will need a code the next time you sign in.')
      await reload()
    } catch (err) {
      fail(err, 'That code could not be confirmed.')
      setCode('')
    } finally {
      setBusy(false)
    }
  }

  async function disable(event: React.FormEvent) {
    event.preventDefault()
    setBusy(true)
    setError('')
    try {
      await api.del('/auth/me/totp', { password })
      setPassword('')
      setRemoving(false)
      setNotice('Two-step verification is off.')
      await reload()
    } catch (err) {
      fail(err, 'The authenticator could not be removed.')
    } finally {
      setBusy(false)
    }
  }

  const enabled = user?.totp_enabled === true

  return (
    <section className="space-y-4 rounded-lg border border-border p-4">
      <div>
        <h2 className="text-sm font-medium">Two-step verification</h2>
        <p className="mt-1 text-xs text-muted">
          A 6-digit code from an authenticator app, asked for after your password. It protects the
          account even if the password is known.
        </p>
      </div>

      {error !== '' && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}
      {notice !== '' && <p className="text-sm text-muted">{notice}</p>}

      {enabled ? (
        <div className="space-y-3">
          <p className="text-sm">
            <span className="rounded-full bg-accent/10 px-2 py-0.5 text-xs text-accent">On</span>
            <span className="ml-2 text-muted">An authenticator app is enrolled.</span>
          </p>

          {removing ? (
            <form onSubmit={disable} className="space-y-2 rounded-md border border-danger/50 p-3">
              {/* The password is the point: without it, an unlocked screen is
                  enough to strip the factor off the account. */}
              <p className="text-xs">
                Removing it means your password alone will sign you in. Confirm with your password.
              </p>
              <input
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                placeholder="Your password"
                className="w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm"
              />
              <div className="flex gap-2">
                <button
                  type="submit"
                  disabled={busy || password === ''}
                  className="rounded-md bg-danger px-3 py-1 text-sm text-white disabled:opacity-50"
                >
                  Remove
                </button>
                <button
                  type="button"
                  onClick={() => {
                    setRemoving(false)
                    setPassword('')
                  }}
                  className="rounded-md border border-border px-3 py-1 text-sm hover:bg-surface-inset"
                >
                  Cancel
                </button>
              </div>
            </form>
          ) : (
            <button
              type="button"
              onClick={() => setRemoving(true)}
              className="rounded-md border border-danger/50 px-3 py-1.5 text-sm text-danger"
            >
              Remove authenticator
            </button>
          )}
        </div>
      ) : enrollment ? (
        <form onSubmit={confirm} className="space-y-3">
          <p className="text-sm">
            Scan this with your authenticator app, then enter the code it shows.
          </p>
          {qr !== '' && (
            <img
              src={qr}
              alt="QR code for enrolling this account in an authenticator app"
              className="rounded-md border border-border bg-white p-2"
              width={200}
              height={200}
            />
          )}

          <div className="space-y-1">
            <span className="text-xs text-muted">Or type this key in by hand</span>
            <div className="flex items-center gap-2">
              <code className="flex-1 rounded-md border border-border bg-surface-raised px-2 py-1 font-mono text-xs break-all">
                {enrollment.secret}
              </code>
              <button
                type="button"
                onClick={() => void copyText(enrollment.secret).then((ok) => setCopied(ok))}
                className="rounded-md border border-border px-2 py-1 text-xs hover:bg-surface-inset"
              >
                {copied ? 'Copied' : 'Copy'}
              </button>
            </div>
          </div>

          <label className="block space-y-1">
            <span className="text-xs text-muted">Code from the app</span>
            <input
              autoComplete="one-time-code"
              inputMode="numeric"
              maxLength={6}
              value={code}
              onChange={(e) => setCode(e.target.value.replace(/\D/g, ''))}
              className="w-40 rounded-md border border-border bg-surface px-2 py-1.5 text-center font-mono tracking-[0.3em]"
            />
          </label>

          <div className="flex gap-2">
            <button
              type="submit"
              disabled={busy || code.length !== 6}
              className="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50"
            >
              Turn on
            </button>
            <button
              type="button"
              onClick={() => {
                setEnrollment(null)
                setQR('')
                setCode('')
              }}
              className="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-surface-inset"
            >
              Cancel
            </button>
          </div>
        </form>
      ) : (
        <button
          type="button"
          onClick={() => void begin()}
          disabled={busy}
          className="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50"
        >
          Set up two-step verification
        </button>
      )}
    </section>
  )
}

/** The panel in a dialog, opened from the user menu — the same shape as
 *  changing a password, which is the other thing an account does to itself. */
export function TwoFactorDialog({ onClose }: { onClose: () => void }) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-6">
      <div className="absolute inset-0 bg-black/40" onClick={onClose} aria-hidden="true" />
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Two-step verification"
        className="relative max-h-[90vh] w-full max-w-md overflow-y-auto rounded-lg border border-border bg-surface-raised p-4 shadow-xl"
      >
        <TwoFactorPanel />
        <button
          type="button"
          onClick={onClose}
          className="mt-3 w-full rounded-md border border-border px-3 py-1.5 text-sm hover:bg-surface-inset"
        >
          Close
        </button>
      </div>
    </div>
  )
}
