import { createBrowserRouter, Navigate, RouterProvider } from 'react-router-dom'
import { Shell } from './Shell'
import { LoginPage } from '@/features/auth/LoginPage'
import { DashboardPage } from '@/features/dashboard/DashboardPage'
import { VMListPage } from '@/features/inventory/VMListPage'
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

const router = createBrowserRouter([
  {
    path: '/',
    element: <Shell />,
    children: [
      { index: true, element: <DashboardPage /> },
      { path: 'vms', element: <VMListPage /> },
      { path: 'audit', element: <ComingSoon title="Audit log" /> },
      { path: 'platforms', element: <ComingSoon title="Platforms" /> },
      { path: 'users', element: <ComingSoon title="Users & groups" /> },
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
