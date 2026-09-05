import { useEffect, useState } from 'react'
import { NavLink, Outlet, useLocation } from 'react-router-dom'
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

interface NavGroup {
  /** Empty for the items that sit above every heading. */
  section: string
  items: NavItem[]
}

// Navigation is filtered by role for clarity, not for security: every route
// is enforced server-side regardless of what the sidebar shows.
//
// Grouped rather than one list of eleven, because the list had grown past the
// point where somebody scans it — "Storage" and "Settings" are one letter
// apart and nothing but position said they were unrelated.
const NAV: NavGroup[] = [
  {
    section: '',
    items: [{ to: '/', label: 'Dashboard', icon: IconDashboard, visible: can.viewInventory }],
  },
  {
    section: 'Infrastructure',
    items: [
      { to: '/vms', label: 'Inventory', icon: IconInventory, visible: can.viewInventory },
      { to: '/hosts', label: 'Hosts', icon: IconHosts, visible: can.viewInfrastructure },
      { to: '/storage', label: 'Storage', icon: IconStorage, visible: can.viewInfrastructure },
      { to: '/networks', label: 'Networks', icon: IconNetworks, visible: can.viewInfrastructure },
    ],
  },
  {
    section: 'Platform',
    items: [
      { to: '/platforms', label: 'Platforms', icon: IconPlatforms, visible: can.managePlatforms },
      {
        to: '/publishing',
        label: 'Published apps',
        icon: IconPublishing,
        visible: can.publishApps,
      },
      {
        to: '/notifications',
        label: 'Notifications',
        icon: IconNotifications,
        visible: can.manageUsers,
      },
    ],
  },
  {
    section: 'Administration',
    items: [
      { to: '/users', label: 'Users & groups', icon: IconUsers, visible: can.manageUsers },
      { to: '/settings', label: 'Settings', icon: IconSettings, visible: can.manageUsers },
      { to: '/audit', label: 'Audit log', icon: IconAudit, visible: can.viewAudit },
    ],
  },
]

const COLLAPSED_KEY = 'proxui.nav.collapsed'
const SECTIONS_KEY = 'proxui.nav.sections'

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

/** Which section headings are folded shut, remembered on the same terms. */
function useClosedSections(): [Set<string>, (name: string) => void] {
  const [closed, setClosed] = useState<Set<string>>(() => {
    try {
      const raw = window.localStorage.getItem(SECTIONS_KEY)
      const parsed: unknown = raw ? JSON.parse(raw) : []
      return new Set(Array.isArray(parsed) ? parsed.filter((v) => typeof v === 'string') : [])
    } catch {
      return new Set()
    }
  })

  const toggle = (name: string) => {
    setClosed((previous) => {
      const next = new Set(previous)
      if (!next.delete(name)) next.add(name)
      try {
        window.localStorage.setItem(SECTIONS_KEY, JSON.stringify([...next]))
      } catch {
        // Remembered, not required.
      }
      return next
    })
  }

  return [closed, toggle]
}

export function Shell() {
  const { user, logout } = useAuth()
  const [changingPassword, setChangingPassword] = useState(false)
  const [managingTwoFactor, setManagingTwoFactor] = useState(false)
  const [collapsed, toggleCollapsed] = useCollapsed()
  const [closedSections, toggleSection] = useClosedSections()
  const [menuOpen, setMenuOpen] = useState(false)
  const branding = useBranding()
  const location = useLocation()

  // Navigating closes the phone menu. Otherwise it stays over the page the tap
  // just loaded, and the only way out is the close button.
  useEffect(() => setMenuOpen(false), [location.pathname])

  if (!user) return null

  const name = branding['branding.portal_name']
  const account = (
    <UserMenu
      user={user}
      compact={collapsed}
      openUp
      onChangePassword={() => setChangingPassword(true)}
      onTwoFactor={() => setManagingTwoFactor(true)}
      onSignOut={() => void logout()}
    />
  )

  return (
    <div className="flex h-full">
      {/* The sidebar is the shell on a desktop; on a phone it is the drawer
          below, and this is hidden rather than squeezed. */}
      <aside
        className={`hidden shrink-0 flex-col border-r border-border-strong bg-surface-raised transition-[width] duration-150 ease-out motion-reduce:transition-none md:flex ${
          collapsed ? 'w-[4.25rem]' : 'w-56'
        }`}
      >
        <Brand
          name={name}
          logo={branding['branding.logo']}
          collapsed={collapsed}
          onToggle={toggleCollapsed}
        />

        <nav
          aria-label="Main"
          id="nav-items"
          className={`flex-1 overflow-y-auto pb-4 ${collapsed ? 'px-1.5' : 'px-2'}`}
        >
          {NAV.map((group) => (
            <NavSection
              key={group.section}
              group={group}
              role={user.role}
              collapsed={collapsed}
              closed={closedSections.has(group.section)}
              onToggle={() => toggleSection(group.section)}
            />
          ))}
        </nav>

        <div className={`border-t border-border-strong py-3 ${collapsed ? 'px-1.5' : 'px-3'}`}>
          {account}
        </div>
      </aside>

      {menuOpen && (
        <div className="fixed inset-0 z-40 md:hidden">
          <div
            className="absolute inset-0 bg-black/40"
            onClick={() => setMenuOpen(false)}
            aria-hidden="true"
          />
          <aside className="absolute left-0 top-0 flex h-full w-64 flex-col border-r border-border-strong bg-surface-raised">
            <div className="flex items-center justify-between px-4 py-4">
              <BrandMark name={name} logo={branding['branding.logo']} />
              <button
                type="button"
                onClick={() => setMenuOpen(false)}
                aria-label="Close menu"
                className="rounded px-1.5 text-lg leading-none text-muted hover:text-content"
              >
                ✕
              </button>
            </div>
            <nav aria-label="Main" className="flex-1 overflow-y-auto px-2 pb-4">
              {NAV.map((group) => (
                <NavSection key={group.section} group={group} role={user.role} />
              ))}
            </nav>
            <div className="border-t border-border px-3 py-3">{account}</div>
          </aside>
        </div>
      )}

      <div className="flex min-w-0 flex-1 flex-col">
        {/* The phone's title bar, and the only way to the menu there. */}
        <div className="flex items-center gap-3 border-b border-border-strong bg-surface-raised px-4 py-3 md:hidden">
          <button
            type="button"
            onClick={() => setMenuOpen(true)}
            aria-label="Open menu"
            className="rounded px-2 py-1 text-lg leading-none text-content-soft"
          >
            ☰
          </button>
          <BrandMark name={name} logo={branding['branding.logo']} />
        </div>

        <main className="min-w-0 flex-1 overflow-auto">
          <div className="mx-auto max-w-[1400px] px-5 py-6 md:px-8 md:py-7">
            <Outlet />
          </div>
        </main>
      </div>

      {managingTwoFactor && <TwoFactorDialog onClose={() => setManagingTwoFactor(false)} />}

      {changingPassword && (
        <ChangePasswordDialog
          onClose={() => setChangingPassword(false)}
          // The change revoked every session, this one included, so the only
          // coherent next step is the login page.
          onChanged={() => void logout()}
        />
      )}
    </div>
  )
}

/** The portal's name and what it is, above the navigation. */
function Brand({
  name,
  logo,
  collapsed,
  onToggle,
}: {
  name: string
  logo: string
  collapsed: boolean
  onToggle: () => void
}) {
  return (
    <div
      className={`flex py-4 ${collapsed ? 'flex-col items-center gap-2' : 'items-center justify-between px-4'}`}
    >
      {collapsed ? (
        <span aria-hidden="true" className="font-mono text-sm font-semibold">
          {initials(name)}
        </span>
      ) : (
        <div className="flex min-w-0 items-center gap-2">
          {logo && <img src={logo} alt="" aria-hidden="true" className="h-6 w-auto shrink-0" />}
          <div className="min-w-0">
            <div className="truncate font-mono text-sm font-semibold tracking-tight">{name}</div>
            <div className="font-mono text-[10px] uppercase tracking-[0.14em] text-accent">
              VM access portal
            </div>
          </div>
        </div>
      )}

      <button
        type="button"
        onClick={onToggle}
        aria-expanded={!collapsed}
        aria-controls="nav-items"
        title={collapsed ? 'Expand menu' : 'Collapse menu'}
        className="shrink-0 rounded p-1 text-muted hover:text-content"
      >
        <IconChevronLeft
          className={`h-4 w-4 transition-transform duration-150 motion-reduce:transition-none ${
            collapsed ? 'rotate-180' : ''
          }`}
        />
        <span className="sr-only">{collapsed ? 'Expand menu' : 'Collapse menu'}</span>
      </button>
    </div>
  )
}

/** The name alone, for the phone bar and the drawer's head. */
function BrandMark({ name, logo }: { name: string; logo: string }) {
  return (
    <span className="flex min-w-0 items-center gap-2">
      {logo && <img src={logo} alt="" aria-hidden="true" className="h-5 w-auto shrink-0" />}
      <span className="truncate font-mono text-sm font-semibold tracking-tight">{name}</span>
    </span>
  )
}

function NavSection({
  group,
  role,
  collapsed = false,
  closed = false,
  onToggle,
}: {
  group: NavGroup
  role: Role
  collapsed?: boolean
  closed?: boolean
  onToggle?: () => void
}) {
  const items = group.items.filter((item) => item.visible(role))
  if (items.length === 0) return null

  // In the icon rail every item shows whatever the section says: there is no
  // heading to fold it under, so folding would just hide links with no way
  // back to them.
  const showItems = collapsed || !closed

  return (
    <div className="mb-3">
      {group.section && !collapsed && (
        <button
          type="button"
          onClick={onToggle}
          aria-expanded={!closed}
          className="flex w-full items-center gap-1 px-3 pb-1 font-mono text-[10px] uppercase tracking-[0.12em] text-muted hover:text-content-soft"
        >
          <span
            aria-hidden="true"
            className={`inline-block w-2 text-[8px] transition-transform motion-reduce:transition-none ${
              closed ? '' : 'rotate-90'
            }`}
          >
            ▶
          </span>
          {group.section}
        </button>
      )}

      {showItems && (
        <ul>
          {items.map((item) => (
            <li key={item.to}>
              <NavLink
                to={item.to}
                end={item.to === '/'}
                // A native tooltip is what names the target once the label is
                // hidden. The label itself stays in the DOM either way, so the
                // link never loses its accessible name.
                title={collapsed ? item.label : undefined}
                className={({ isActive }) =>
                  `flex items-center rounded-sm text-sm ${
                    collapsed ? 'justify-center px-0 py-2' : 'gap-2.5 px-3 py-1.5'
                  } ${
                    isActive
                      ? 'bg-accent-wash font-medium text-accent-strong'
                      : 'text-content-soft hover:bg-surface-inset'
                  }`
                }
              >
                <item.icon className="h-4 w-4 shrink-0" />
                <span className={collapsed ? 'sr-only' : 'truncate'}>{item.label}</span>
              </NavLink>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

// Two letters for the icon rail, taken from whatever the portal is called.
// Not an abbreviation anybody chose — just enough to hold the space and say
// which portal this is when two are open in different tabs.
function initials(name: string): string {
  const words = name
    .trim()
    .split(/[\s._-]+/)
    .filter(Boolean)
  if (words.length === 0) return '··'
  if (words.length === 1) return words[0].slice(0, 2).toLowerCase()
  return (words[0][0] + words[1][0]).toLowerCase()
}
