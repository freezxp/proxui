import { lazy, Suspense } from 'react'
import { createBrowserRouter, Navigate, RouterProvider } from 'react-router-dom'
import { Shell } from './Shell'
import { LoginPage } from '@/features/auth/LoginPage'
import { DashboardPage } from '@/features/dashboard/DashboardPage'
import { VMListPage } from '@/features/inventory/VMListPage'
import { VMDetailPage } from '@/features/inventory/VMDetailPage'
import { PlatformsPage } from '@/features/platforms/PlatformsPage'
import { UsersPage } from '@/features/admin/UsersPage'
import { useAuth } from '@/features/auth/useAuth'

/** Placeholder for pages arriving in later sprints, so navigation is
 *  explorable now without pretending the features exist. */
function ComingSoon({ title }: { title: string }) {
  return (
    <div className="space-y-2">
      <h1 className="text-xl font-semibold">{title}</h1>
      <p className="text-sm text-muted">
        The API for this page is live; the interface arrives in a later sprint.
      </p>
    </div>
  )
}

// noVNC is the single largest dependency in the app and only a console needs
// it, so it is fetched when one is opened (NFR-P5).
const ConsolePage = lazy(() =>
  import('@/features/console/ConsolePage').then((m) => ({ default: m.ConsolePage })),
)

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
  {
    path: '/',
    element: <Shell />,
    children: [
      { index: true, element: <DashboardPage /> },
      { path: 'vms', element: <VMListPage /> },
      { path: 'vms/:vmId', element: <VMDetailPage /> },
      { path: 'audit', element: <ComingSoon title="Audit log" /> },
      { path: 'platforms', element: <PlatformsPage /> },
      { path: 'users', element: <UsersPage /> },
      { path: '*', element: <Navigate to="/" replace /> },
    ],
  },
])

export function AppRoutes() {
  const { user, loading } = useAuth()

  // Waiting for the refresh attempt avoids flashing the login form at a user
  // whose session is about to be restored.
  if (loading) {
    return (
      <div className="flex min-h-full items-center justify-center">
        <p className="text-sm text-muted">Loading…</p>
      </div>
    )
  }

  if (!user) return <LoginPage />

  return <RouterProvider router={router} />
}
