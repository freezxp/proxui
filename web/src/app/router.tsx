import { lazy, Suspense, useState } from 'react'
import { createBrowserRouter, Navigate, RouterProvider } from 'react-router-dom'
import { Shell } from './Shell'
import { LoginPage } from '@/features/auth/LoginPage'
import { ChangePasswordDialog } from '@/features/auth/ChangePasswordDialog'
import { RegisterPage } from '@/features/auth/RegisterPage'
import { WelcomePage } from '@/features/auth/WelcomePage'
import { DashboardPage } from '@/features/dashboard/DashboardPage'
import { VMListPage } from '@/features/inventory/VMListPage'
import { VMDetailPage } from '@/features/inventory/VMDetailPage'
import { PlatformsPage } from '@/features/platforms/PlatformsPage'
import { PublishingPage } from '@/features/publishing/PublishingPage'
import { ContainerAppsPage } from '@/features/containerapps/ContainerAppsPage'
import { UsersPage } from '@/features/admin/UsersPage'
import { AuditPage } from '@/features/audit/AuditPage'
import { SettingsPage } from '@/features/admin/SettingsPage'
import { NotificationsPage } from '@/features/notifications/NotificationsPage'
import { HostsPage, NetworksPage, StoragePage } from '@/features/infrastructure/InfrastructurePage'
import { useAuth } from '@/features/auth/useAuth'
import { can } from '@/lib/permissions'

// noVNC is the single largest dependency in the app and only a console needs
// it, so it is fetched when one is opened (NFR-P5).
const ConsolePage = lazy(() =>
  import('@/features/console/ConsolePage').then((m) => ({ default: m.ConsolePage })),
)

// xterm.js is the same story as noVNC: only an SSH session needs it, so it is
// fetched when one is opened (NFR-P5).
const SshPage = lazy(() => import('@/features/shell/SshPage').then((m) => ({ default: m.SshPage })))

function LazySsh() {
  return (
    <Suspense
      fallback={
        <div className="flex h-screen items-center justify-center bg-[#0b0f17] text-sm text-white/70">
          Loading terminal…
        </div>
      }
    >
      <SshPage />
    </Suspense>
  )
}

function LazyConsole() {
  return (
    <Suspense
      fallback={
        <div className="flex h-screen items-center justify-center bg-black text-sm text-white/70">
          Loading console…
        </div>
      }
    >
      <ConsolePage />
    </Suspense>
  )
}

const router = createBrowserRouter([
  // The console owns the whole viewport: no sidebar, no top bar, so it lives
  // beside the shell rather than inside it (docs/13-ui-design.md §13.2).
  { path: '/console/:vmId', element: <LazyConsole /> },
  // The SSH terminal owns the viewport for the same reason, and lives in its
  // own tab so navigating the portal cannot tear down a live session.
  { path: '/ssh/:vmId', element: <LazySsh /> },
  {
    path: '/',
    element: <Shell />,
    children: [
      { index: true, element: <DashboardPage /> },
      { path: 'vms', element: <VMListPage /> },
      { path: 'vms/:vmId', element: <VMDetailPage /> },
      { path: 'audit', element: <AuditPage /> },
      { path: 'hosts', element: <HostsPage /> },
      { path: 'storage', element: <StoragePage /> },
      { path: 'networks', element: <NetworksPage /> },
      { path: 'notifications', element: <NotificationsPage /> },
      { path: 'platforms', element: <PlatformsPage /> },
      { path: 'publishing', element: <PublishingPage /> },
      { path: 'container-apps', element: <ContainerAppsPage /> },
      { path: 'users', element: <UsersPage /> },
      { path: 'settings', element: <SettingsPage /> },
      { path: '*', element: <Navigate to="/" replace /> },
    ],
  },
])

export function AppRoutes() {
  const { user, loading, logout } = useAuth()
  const [registering, setRegistering] = useState(false)

  // Waiting for the refresh attempt avoids flashing the login form at a user
  // whose session is about to be restored.
  if (loading) {
    return (
      <div className="flex min-h-full items-center justify-center">
        <p className="text-sm text-muted">Loading…</p>
      </div>
    )
  }

  if (!user) {
    return registering ? (
      <RegisterPage onDone={() => setRegistering(false)} onCancel={() => setRegistering(false)} />
    ) : (
      <LoginPage onRegister={() => setRegistering(true)} />
    )
  }

  // An account with a temporary password reaches nothing else until it has
  // one of its own. The server would still enforce every route; this is so
  // the person is told why rather than left wondering.
  if (user.must_change_password) {
    return <ChangePasswordDialog forced onChanged={() => void logout()} />
  }

  // An account with no permissions gets one page rather than a shell full of
  // links that would each refuse it.
  if (!can.useThePortal(user.role)) return <WelcomePage />

  return <RouterProvider router={router} />
}
