import { useState } from 'react'
import { NavLink, Outlet } from 'react-router-dom'
import { useAuth } from '@/features/auth/useAuth'
import { can } from '@/lib/permissions'
import { useBranding } from '@/lib/branding'
import { UserMenu } from '@/components/UserMenu'
import { ChangePasswordDialog } from '@/features/auth/ChangePasswordDialog'
import { TwoFactorDialog } from '@/features/auth/TwoFactorPanel'
import {
  IconAudit,
  IconChevronLeft,
  IconDashboard,
  IconHosts,
  IconInventory,
  IconNetworks,
  IconNotifications,
  IconPlatforms,
  IconPublishing,
  IconSettings,
  IconStorage,
  IconUsers,
  type Icon,
} from '@/components/icons'
import type { Role } from '@/api/types'

interface NavItem {
  to: string
  label: string
  icon: Icon
  visible: (role: Role) => boolean
}

// Navigation is filtered by role for clarity, not for security: every route
// is enforced server-side regardless of what the sidebar shows.
const NAV: NavItem[] = [
  { to: '/', label: 'Dashboard', icon: IconDashboard, visible: can.viewInventory },
  { to: '/vms', label: 'Inventory', icon: IconInventory, visible: can.viewInventory },
  { to: '/hosts', label: 'Hosts', icon: IconHosts, visible: can.viewInfrastructure },
  { to: '/storage', label: 'Storage', icon: IconStorage, visible: can.viewInfrastructure },
  { to: '/networks', label: 'Networks', icon: IconNetworks, visible: can.viewInfrastructure },
  { to: '/audit', label: 'Audit log', icon: IconAudit, visible: can.viewAudit },
  { to: '/platforms', label: 'Platforms', icon: IconPlatforms, visible: can.managePlatforms },
  { to: '/publishing', label: 'Published apps', icon: IconPublishing, visible: can.publishApps },
  {
    to: '/notifications',
    label: 'Notifications',
    icon: IconNotifications,
    visible: can.manageUsers,
  },
  { to: '/users', label: 'Users & groups', icon: IconUsers, visible: can.manageUsers },
  { to: '/settings', label: 'Settings', icon: IconSettings, visible: can.manageUsers },
]

const COLLAPSED_KEY = 'proxui.nav.collapsed'

/**
 * Remembers whether the sidebar is collapsed.
 *
 * Kept in localStorage rather than in the URL or on the account: it is a
 * property of this screen, not of this person — someone on a laptop and a wide
 * monitor wants a different answer on each, and syncing it would fight them.
 * Storage is wrapped because it throws outright in some private-browsing
 * modes, and a nav that cannot remember its width is better than one that
 * crashes the shell.
 */
function useCollapsed(): [boolean, () => void] {
  const [collapsed, setCollapsed] = useState(() => {
    try {
      return window.localStorage.getItem(COLLAPSED_KEY) === '1'
    } catch {
      return false
    }
  })

  const toggle = () => {
    setCollapsed((previous) => {
      const next = !previous
      try {
        window.localStorage.setItem(COLLAPSED_KEY, next ? '1' : '0')
      } catch {
        // Not being able to remember the choice is not a reason to refuse it.
      }
      return next
    })
  }

  return [collapsed, toggle]
}

export function Shell() {
  const { user, logout } = useAuth()
  const [changingPassword, setChangingPassword] = useState(false)
  const [managingTwoFactor, setManagingTwoFactor] = useState(false)
  const [collapsed, toggleCollapsed] = useCollapsed()
  const branding = useBranding()
  if (!user) return null

  return (
    <div className="flex h-full flex-col">
      <header className="flex items-center justify-between border-b border-border bg-surface-raised px-4 py-3">
        <div className="flex items-center gap-3">
          {branding['branding.logo'] && (
            <img
              src={branding['branding.logo']}
              alt=""
              className="h-7 w-auto"
              // Decorative: the portal name beside it already says what this
              // is, so a screen reader repeating it would be noise.
              aria-hidden="true"
            />
          )}
          <span className="text-lg font-semibold">{branding['branding.portal_name']}</span>
          <span className="hidden text-sm text-muted sm:inline">VM access portal</span>
        </div>

        <UserMenu
          user={user}
          onChangePassword={() => setChangingPassword(true)}
          onTwoFactor={() => setManagingTwoFactor(true)}
          onSignOut={() => void logout()}
        />
      </header>

      {managingTwoFactor && <TwoFactorDialog onClose={() => setManagingTwoFactor(false)} />}

      {changingPassword && (
        <ChangePasswordDialog
          onClose={() => setChangingPassword(false)}
          // The change revoked every session, this one included, so the only
          // coherent next step is the login page.
          onChanged={() => void logout()}
        />
      )}

      <div className="flex min-h-0 flex-1">
        <nav
          aria-label="Main"
          className={`flex shrink-0 flex-col border-r border-border bg-surface-raised p-3 transition-[width] duration-150 ease-out motion-reduce:transition-none ${
            collapsed ? 'w-[4.25rem]' : 'w-52'
          }`}
        >
          <ul id="nav-items" className="space-y-1">
            {NAV.filter((item) => item.visible(user.role)).map((item) => (
              <li key={item.to}>
                <NavLink
                  to={item.to}
                  end={item.to === '/'}
                  // A native tooltip is what names the target once the label is
                  // hidden. The label itself stays in the DOM either way, so
                  // the link never loses its accessible name.
                  title={collapsed ? item.label : undefined}
                  className={({ isActive }) =>
                    `flex items-center gap-3 rounded-md py-2 text-sm ${
                      collapsed ? 'justify-center px-0' : 'px-3'
                    } ${
                      isActive
                        ? 'bg-accent/10 font-medium text-accent'
                        : 'text-content hover:bg-surface'
                    }`
                  }
                >
                  <item.icon className="h-5 w-5 shrink-0" />
                  <span className={collapsed ? 'sr-only' : 'truncate'}>{item.label}</span>
                </NavLink>
              </li>
            ))}
          </ul>

          <button
            type="button"
            onClick={toggleCollapsed}
            aria-expanded={!collapsed}
            aria-controls="nav-items"
            className={`mt-auto flex items-center gap-3 rounded-md py-2 text-sm text-muted hover:bg-surface hover:text-content ${
              collapsed ? 'justify-center px-0' : 'px-3'
            }`}
          >
            <IconChevronLeft
              className={`h-5 w-5 shrink-0 transition-transform duration-150 motion-reduce:transition-none ${
                collapsed ? 'rotate-180' : ''
              }`}
            />
            <span className={collapsed ? 'sr-only' : 'truncate'}>
              {collapsed ? 'Expand menu' : 'Collapse menu'}
            </span>
          </button>
        </nav>

        <main className="min-w-0 flex-1 overflow-auto p-6">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
