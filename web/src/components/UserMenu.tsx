import { useEffect, useRef, useState } from 'react'
import {
  applyMode,
  applyTheme,
  storedMode,
  storedTheme,
  watchSystemMode,
  type Mode,
  type Theme,
} from '@/lib/theme'
import { roleLabel } from '@/lib/permissions'
import type { CurrentUser } from '@/api/types'

const MODES: { value: Mode; label: string }[] = [
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
  { value: 'system', label: 'System' },
]

// Two palettes, each defined in both modes: picking one does not pick light or
// dark, and picking dark does not pick a palette.
const THEMES: { value: Theme; label: string; hint: string }[] = [
  { value: 'slate', label: 'Slate', hint: 'cool grey, teal accent, IBM Plex' },
  { value: 'classic', label: 'Classic', hint: 'the original blue and slate' },
]

/** Everything that belongs to the person rather than the estate: who they are,
 *  how the portal looks to them, their password, and the way out. One control
 *  instead of four, because none of them is used often enough to earn
 *  permanent space in the header. */
export function UserMenu({
  user,
  onChangePassword,
  onTwoFactor,
  onSignOut,
  compact = false,
  openUp = false,
}: {
  user: CurrentUser
  onChangePassword: () => void
  onTwoFactor: () => void
  onSignOut: () => void
  /** Show the initials alone, for the sidebar's icon rail. */
  compact?: boolean
  /** Open above the trigger, for a control that sits at the foot of the
   *  sidebar rather than in a header. */
  openUp?: boolean
}) {
  const [open, setOpen] = useState(false)
  const [mode, setMode] = useState<Mode>(storedMode)
  const [theme, setTheme] = useState<Theme>(storedTheme)
  const container = useRef<HTMLDivElement>(null)
  const trigger = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    applyMode(mode)
    return watchSystemMode()
  }, [mode])

  useEffect(() => applyTheme(theme), [theme])

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
        className={`flex w-full items-center rounded-sm text-sm hover:bg-surface-inset ${
          compact ? 'justify-center px-0 py-1.5' : 'gap-2 px-2 py-1.5'
        }`}
      >
        <span
          aria-hidden="true"
          className="flex h-6 w-6 shrink-0 items-center justify-center rounded-full bg-accent-wash text-[10px] font-medium text-accent-strong"
        >
          {initials(name)}
        </span>
        {!compact && (
          <>
            <span className="min-w-0 flex-1 truncate text-left">{name}</span>
            <span
              aria-hidden="true"
              className={`shrink-0 text-muted transition-transform ${open ? 'rotate-180' : ''}`}
            >
              ▾
            </span>
          </>
        )}
      </button>

      {open && (
        <div
          role="menu"
          aria-label="Account"
          className={`absolute left-0 z-40 w-60 overflow-hidden rounded-md border border-border bg-surface-raised shadow-lg ${
            openUp ? 'bottom-full mb-1' : 'right-0 mt-1'
          }`}
        >
          <div className="border-b border-border px-3 py-2">
            <div className="truncate text-sm font-medium">{name}</div>
            <div className="truncate text-xs text-muted">{user.email}</div>
            <div className="mt-1 text-xs text-muted">{roleLabel(user.role)}</div>
          </div>

          <div className="space-y-2 border-b border-border px-3 py-2">
            <div>
              <div className="mb-1 text-xs uppercase tracking-wide text-muted">Appearance</div>
              {/* A segmented control rather than a select: three options, and
                  seeing which one is active is the whole point. */}
              <div className="flex gap-1">
                {MODES.map((option) => (
                  <button
                    key={option.value}
                    onClick={() => setMode(option.value)}
                    aria-pressed={mode === option.value}
                    className={`flex-1 rounded-md px-2 py-1 text-xs ${
                      mode === option.value
                        ? 'bg-accent text-white'
                        : 'border border-border hover:bg-surface-inset'
                    }`}
                  >
                    {option.label}
                  </button>
                ))}
              </div>
            </div>

            <div>
              <div className="mb-1 text-xs uppercase tracking-wide text-muted">Theme</div>
              {/* Stacked rather than segmented: each of these needs a line
                  saying what it is, and two words on a chip cannot. */}
              <div className="space-y-1">
                {THEMES.map((option) => (
                  <button
                    key={option.value}
                    onClick={() => setTheme(option.value)}
                    aria-pressed={theme === option.value}
                    className={`block w-full rounded-md px-2 py-1 text-left text-xs ${
                      theme === option.value
                        ? 'bg-accent-wash text-accent-strong'
                        : 'border border-border hover:bg-surface-inset'
                    }`}
                  >
                    <span className="font-medium">{option.label}</span>
                    <span className="block text-[10px] text-muted">{option.hint}</span>
                  </button>
                ))}
              </div>
            </div>
          </div>

          <MenuItem onClick={choose(onChangePassword)}>Change password</MenuItem>
          <MenuItem onClick={choose(onTwoFactor)}>
            Two-step verification
            {user.totp_enabled && (
              <span className="ml-2 rounded-full bg-accent/10 px-1.5 py-0.5 text-[10px] text-accent">
                on
              </span>
            )}
          </MenuItem>
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
      className={`block w-full px-3 py-2 text-left text-sm hover:bg-surface-inset ${
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
