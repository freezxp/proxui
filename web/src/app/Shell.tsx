import { useState } from 'react'
import { NavLink, Outlet } from 'react-router-dom'
import { useAuth } from '@/features/auth/useAuth'
import { can, roleLabel } from '@/lib/permissions'
import { ThemeToggle } from '@/components/ThemeToggle'
import { ChangePasswordDialog } from '@/features/auth/ChangePasswordDialog'
import type { Role } from '@/api/types'

interface NavItem {
  to: string
  label: string
  visible: (role: Role) => boolean
}

// Navigation is filtered by role for clarity, not for security: every route
// is enforced server-side regardless of what the sidebar shows.
const NAV: NavItem[] = [
  { to: '/', label: 'Dashboard', visible: can.viewInventory },
  { to: '/vms', label: 'Inventory', visible: can.viewInventory },
  { to: '/hosts', label: 'Hosts', visible: can.viewInfrastructure },
  { to: '/storage', label: 'Storage', visible: can.viewInfrastructure },
  { to: '/networks', label: 'Networks', visible: can.viewInfrastructure },
  { to: '/audit', label: 'Audit log', visible: can.viewAudit },
  { to: '/platforms', label: 'Platforms', visible: can.managePlatforms },
  { to: '/notifications', label: 'Notifications', visible: can.manageUsers },
  { to: '/users', label: 'Users & groups', visible: can.manageUsers },
  { to: '/settings', label: 'Settings', visible: can.manageUsers },
]

export function Shell() {
  const { user, logout } = useAuth()
  const [changingPassword, setChangingPassword] = useState(false)
  if (!user) return null

  return (
    <div className="flex h-full flex-col">
      <header className="flex items-center justify-between border-b border-border bg-surface-raised px-4 py-3">
        <div className="flex items-center gap-3">
          <span className="text-lg font-semibold">ProxUI</span>
          <span className="hidden text-sm text-muted sm:inline">VM access portal</span>
        </div>

        <div className="flex items-center gap-4">
          <ThemeToggle />
          <div className="text-right text-sm leading-tight">
            <div className="font-medium">{user.display_name || user.username}</div>
            <div className="text-xs text-muted">{roleLabel(user.role)}</div>
          </div>
          <button
            onClick={() => setChangingPassword(true)}
            className="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-surface"
          >
            Change password
          </button>
          <button
            onClick={() => void logout()}
            className="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-surface"
          >
            Sign out
          </button>
        </div>
      </header>

      {changingPassword && (
        <ChangePasswordDialog
          onClose={() => setChangingPassword(false)}
          // The change revoked every session, this one included, so the only
          // coherent next step is the login page.
          onChanged={() => void logout()}
        />
      )}

      <div className="flex min-h-0 flex-1">
        <nav className="w-52 shrink-0 border-r border-border bg-surface-raised p-3">
          <ul className="space-y-1">
            {NAV.filter((item) => item.visible(user.role)).map((item) => (
              <li key={item.to}>
                <NavLink
                  to={item.to}
                  end={item.to === '/'}
                  className={({ isActive }) =>
                    `block rounded-md px-3 py-2 text-sm ${
                      isActive
                        ? 'bg-accent/10 font-medium text-accent'
                        : 'text-content hover:bg-surface'
                    }`
                  }
                >
                  {item.label}
                </NavLink>
              </li>
            ))}
          </ul>
        </nav>

        <main className="min-w-0 flex-1 overflow-auto p-6">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
