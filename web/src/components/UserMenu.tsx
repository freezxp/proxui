import { useEffect, useRef, useState } from 'react'
import { applyTheme, storedTheme, watchSystemTheme, type Theme } from '@/lib/theme'
import { roleLabel } from '@/lib/permissions'
import type { CurrentUser } from '@/api/types'

const THEMES: { value: Theme; label: string }[] = [
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
  { value: 'system', label: 'System' },
]

/** Everything that belongs to the person rather than the estate: who they are,
 *  how the portal looks to them, their password, and the way out. One control
 *  instead of four, because none of them is used often enough to earn
 *  permanent space in the header. */
export function UserMenu({
  user,
  onChangePassword,
  onSignOut,
}: {
  user: CurrentUser
  onChangePassword: () => void
  onSignOut: () => void
}) {
  const [open, setOpen] = useState(false)
  const [theme, setTheme] = useState<Theme>(storedTheme)
  const container = useRef<HTMLDivElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    applyTheme(theme)
    return watchSystemTheme()
  }, [theme])

  useEffect(() => {
    if (!open) return

    function onPointerDown(event: MouseEvent) {
      if (!container.current?.contains(event.target as Node)) setOpen(false)
    }
    function onKeyDown(event: KeyboardEvent) {
      if (event.key !== 'Escape') return
      setOpen(false)
      // Escape should leave focus where the reader can carry on, which is the
      // control they opened rather than the top of the document.
      trigger.current?.focus()
    }

    document.addEventListener('mousedown', onPointerDown)
    document.addEventListener('keydown', onKeyDown)
    return () => {
      document.removeEventListener('mousedown', onPointerDown)
      document.removeEventListener('keydown', onKeyDown)
    }
  }, [open])

  const name = user.display_name || user.username

  function choose(action: () => void) {
    return () => {
      setOpen(false)
      action()
    }
  }

  return (
    <div ref={container} className="relative">
      <button
        ref={trigger}
        onClick={() => setOpen((v) => !v)}
        aria-haspopup="menu"
        aria-expanded={open}
        className="flex items-center gap-2 rounded-md border border-border px-2 py-1.5 text-sm hover:bg-surface"
      >
        <span
          aria-hidden="true"
          className="flex h-6 w-6 items-center justify-center rounded-full bg-accent/15 text-xs font-medium text-accent"
        >
          {initials(name)}
        </span>
        <span className="hidden max-w-40 truncate sm:inline">{name}</span>
        <span
          aria-hidden="true"
          className={`text-muted transition-transform ${open ? 'rotate-180' : ''}`}
        >
          ▾
        </span>
      </button>

      {open && (
        <div
          role="menu"
          aria-label="Account"
          className="absolute right-0 z-40 mt-1 w-60 overflow-hidden rounded-lg border border-border bg-surface shadow-lg"
        >
          <div className="border-b border-border px-3 py-2">
            <div className="truncate text-sm font-medium">{name}</div>
            <div className="truncate text-xs text-muted">{user.email}</div>
            <div className="mt-1 text-xs text-muted">{roleLabel(user.role)}</div>
          </div>

          <div className="border-b border-border px-3 py-2">
            <div className="mb-1 text-xs uppercase tracking-wide text-muted">Appearance</div>
            {/* A segmented control rather than a select: three options, and
                seeing which one is active is the whole point. */}
            <div className="flex gap-1">
              {THEMES.map((option) => (
                <button
                  key={option.value}
                  onClick={() => setTheme(option.value)}
                  aria-pressed={theme === option.value}
                  className={`flex-1 rounded-md px-2 py-1 text-xs ${
                    theme === option.value
                      ? 'bg-accent text-white'
                      : 'border border-border hover:bg-surface-raised'
                  }`}
                >
                  {option.label}
                </button>
              ))}
            </div>
          </div>

          <MenuItem onClick={choose(onChangePassword)}>Change password</MenuItem>
          <MenuItem onClick={choose(onSignOut)} tone="danger">
            Sign out
          </MenuItem>
        </div>
      )}
    </div>
  )
}

function MenuItem({
  children,
  onClick,
  tone,
}: {
  children: React.ReactNode
  onClick: () => void
  tone?: 'danger'
}) {
  return (
    <button
      role="menuitem"
      onClick={onClick}
      className={`block w-full px-3 py-2 text-left text-sm hover:bg-surface-raised ${
        tone === 'danger' ? 'text-danger' : ''
      }`}
    >
      {children}
    </button>
  )
}

// Two letters from a display name, or one from a username. Enough to tell two
// people apart at a glance without pretending to be an avatar service.
function initials(name: string): string {
  const parts = name.trim().split(/\s+/).filter(Boolean)
  if (parts.length === 0) return '?'
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase()
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase()
}
