import { useAuth } from './useAuth'
import { useBranding } from '@/lib/branding'

/** Where an account with no access lands.
 *
 *  It exists because the alternative is worse: dropping someone into an empty
 *  dashboard leaves them to conclude the portal is broken, and hunting through
 *  a sidebar of pages that all refuse them is no better. This says plainly
 *  that the account works and is waiting on someone else. */
export function WelcomePage() {
  const { user, logout } = useAuth()
  const branding = useBranding()
  if (!user) return null

  return (
    <div className="flex min-h-full items-center justify-center px-4 py-10">
      <div className="w-full max-w-lg space-y-6 rounded-lg border border-border bg-surface-raised p-8 shadow-sm">
        <div className="flex items-center gap-3">
          {branding['branding.logo'] && (
            <img src={branding['branding.logo']} alt="" aria-hidden="true" className="h-9 w-auto" />
          )}
          <div>
            <h1 className="text-xl font-semibold">{branding['branding.portal_name']}</h1>
            <p className="text-sm text-muted">Signed in as {user.display_name || user.username}</p>
          </div>
        </div>

        <div className="space-y-3">
          <h2 className="font-medium">Your account is ready, but empty</h2>
          <p className="text-sm text-muted">
            You can sign in, and that is all for now. An administrator has to grant your account
            access to machines before anything appears here.
          </p>
          <p className="text-sm text-muted">
            Nothing is wrong and there is nothing to fix at your end — ask whoever runs this portal
            to give you access, and mention the address you signed in with:
          </p>
          <p className="rounded-md border border-border bg-surface px-3 py-2 font-mono text-sm">
            {user.email}
          </p>
        </div>

        <div className="flex items-center gap-3 border-t border-border pt-4">
          <button
            onClick={() => void logout()}
            className="rounded-md border border-border px-3 py-2 text-sm hover:bg-surface-inset"
          >
            Sign out
          </button>
          <span className="text-xs text-muted">
            This page will be replaced by the portal once access is granted.
          </span>
        </div>
      </div>
    </div>
  )
}
