import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

/**
 * Deleting an account from the users table. The parts worth holding still are
 * the guards around the button rather than the request it makes: an
 * administrator who deletes the wrong row has no way back, and one who deletes
 * their own has no way in.
 */

const apiGet = vi.fn()
const apiDel = vi.fn()

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: {
      get: (...args: unknown[]) => apiGet(...args),
      post: vi.fn(),
      put: vi.fn(),
      del: (...args: unknown[]) => apiDel(...args),
    },
  }
})

vi.mock('@/features/auth/useAuth', () => ({
  useAuth: () => ({ user: { id: 'me', username: 'root', role: 'admin' } }),
}))

const { UsersPage } = await import('./UsersPage')

const USERS = [
  {
    id: 'me',
    username: 'root',
    email: 'root@example.test',
    display_name: 'Root',
    role: 'admin',
    is_active: true,
    totp_enabled: false,
    must_change_password: false,
    groups: [],
    created_at: '2026-01-01T00:00:00Z',
    last_login_at: null,
  },
  {
    id: 'u2',
    username: 'leaver',
    email: 'leaver@example.test',
    display_name: 'A Leaver',
    role: 'readonly',
    is_active: true,
    totp_enabled: false,
    must_change_password: false,
    groups: [],
    created_at: '2026-01-01T00:00:00Z',
    last_login_at: null,
  },
]

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <UsersPage />
    </QueryClientProvider>,
  )
}

beforeEach(() => {
  apiGet.mockReset()
  apiDel.mockReset()
  apiGet.mockImplementation((path: string) =>
    path === '/users' ? Promise.resolve({ data: USERS }) : Promise.resolve({ data: [] }),
  )
  apiDel.mockResolvedValue(undefined)
})

describe('UsersPage deletion', () => {
  it('offers Delete on other accounts and not on your own', async () => {
    renderPage()
    await screen.findByText('A Leaver')

    // One row is the signed-in administrator; only the other may be deleted.
    expect(screen.getAllByRole('button', { name: 'Delete' })).toHaveLength(1)
  })

  it('holds the deletion until the username is typed', async () => {
    const person = userEvent.setup()
    renderPage()
    await screen.findByText('A Leaver')

    await person.click(screen.getByRole('button', { name: 'Delete' }))
    const confirm = screen.getByRole('button', { name: 'Delete account' })
    expect(confirm).toHaveProperty('disabled', true)

    await person.type(screen.getByLabelText('Type leaver to confirm'), 'leave')
    expect(confirm).toHaveProperty('disabled', true)

    await person.type(screen.getByLabelText('Type leaver to confirm'), 'r')
    expect(confirm).toHaveProperty('disabled', false)

    await person.click(confirm)
    await waitFor(() => expect(apiDel).toHaveBeenCalledWith('/users/u2'))
  })

  it('keeps the dialog open and shows why the server refused', async () => {
    const { ApiError } = await import('@/api/client')
    apiDel.mockRejectedValue(
      new ApiError(409, 'user.last_admin', 'This is the last administrator who can sign in.'),
    )
    const person = userEvent.setup()
    renderPage()
    await screen.findByText('A Leaver')

    await person.click(screen.getByRole('button', { name: 'Delete' }))
    await person.type(screen.getByLabelText('Type leaver to confirm'), 'leaver')
    await person.click(screen.getByRole('button', { name: 'Delete account' }))

    // The reason has to land where the decision was made, not in a toast that
    // outlives the dialog by a second.
    await screen.findByText('This is the last administrator who can sign in.')
    expect(screen.getByRole('dialog', { name: 'Delete user' })).toBeDefined()
  })
})
