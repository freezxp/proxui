import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { ApiError } from '@/api/client'

/**
 * The second factor as the browser sees it (AUTH-04). Two things are worth
 * pinning here and cannot be pinned server-side: that a challenged login shows
 * a code form rather than an error, and that the two ways a code can be
 * refused lead to different places — one keeps the form, the other sends the
 * operator back to the password.
 */

const apiPost = vi.fn()
const apiGet = vi.fn()
const apiDel = vi.fn()
// The provider only loads /auth/me when a refresh succeeds, so the enrolment
// tests - which need a signed-in user - turn this on.
const refreshSession = vi.fn().mockResolvedValue(false)

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    setAccessToken: vi.fn(),
    setUnauthenticatedHandler: vi.fn(),
    refreshSession: () => refreshSession(),
    api: {
      get: (...args: unknown[]) => apiGet(...args),
      post: (...args: unknown[]) => apiPost(...args),
      del: (...args: unknown[]) => apiDel(...args),
      put: vi.fn(),
    },
  }
})

vi.mock('@/lib/branding', () => ({
  useBranding: () => ({ 'branding.portal_name': 'ProxUI' }),
}))
vi.mock('@/lib/authMethods', () => ({
  useAuthMethods: () => ({ google: false, registration: false }),
  SSO_MESSAGES: {},
}))

// qrcode touches a canvas jsdom does not have; the panel only needs a URL.
vi.mock('qrcode', () => ({
  default: { toDataURL: vi.fn().mockResolvedValue('data:image/png;base64,stub') },
}))

const { LoginPage } = await import('./LoginPage')
const { TwoFactorPanel } = await import('./TwoFactorPanel')
const { AuthProvider } = await import('./useAuth')

const CHALLENGE = { mfa_required: true, mfa_token: 'challenge-1', expires_in: 300 }
const TOKEN = { access_token: 'access-1', token_type: 'Bearer', expires_in: 900 }

const USER = {
  id: 'u1',
  username: 'jsmith',
  email: 'jsmith@example.test',
  display_name: 'J Smith',
  role: 'operator',
  totp_enabled: false,
  must_change_password: false,
}

function renderLogin() {
  return render(
    <AuthProvider>
      <LoginPage onRegister={() => {}} />
    </AuthProvider>,
  )
}

async function signIn(user: ReturnType<typeof userEvent.setup>) {
  await user.type(screen.getByLabelText('Username'), 'jsmith')
  await user.type(screen.getByLabelText('Password'), 'correct horse')
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
}

describe('signing in with a second factor', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    apiGet.mockResolvedValue(USER)
    refreshSession.mockResolvedValue(false)
  })

  it('asks for a code instead of reporting a failure', async () => {
    apiPost.mockResolvedValueOnce(CHALLENGE)
    const user = userEvent.setup()
    renderLogin()

    await signIn(user)

    await screen.findByText('Two-step verification')
    expect(screen.queryByRole('alert')).toBeNull()
    // No session was established by the password alone.
    expect(apiGet).not.toHaveBeenCalledWith('/auth/me')
  })

  it('completes the sign-in with the code', async () => {
    apiPost.mockResolvedValueOnce(CHALLENGE).mockResolvedValueOnce(TOKEN)
    const user = userEvent.setup()
    renderLogin()

    await signIn(user)
    await screen.findByText('Two-step verification')
    await user.type(screen.getByLabelText('Code'), '123456')
    await user.click(screen.getByRole('button', { name: 'Verify' }))

    await waitFor(() =>
      expect(apiPost).toHaveBeenCalledWith(
        '/auth/mfa',
        { mfa_token: 'challenge-1', code: '123456' },
        { skipRefresh: true },
      ),
    )
  })

  it('keeps the code form open when a code is simply wrong', async () => {
    apiPost
      .mockResolvedValueOnce(CHALLENGE)
      .mockRejectedValueOnce(new ApiError(401, 'auth.invalid_code', 'nope'))
    const user = userEvent.setup()
    renderLogin()

    await signIn(user)
    await screen.findByText('Two-step verification')
    await user.type(screen.getByLabelText('Code'), '000000')
    await user.click(screen.getByRole('button', { name: 'Verify' }))

    await screen.findByRole('alert')
    // Still on the code step, and the field is cleared for the next attempt.
    expect((screen.getByLabelText('Code') as HTMLInputElement).value).toBe('')
    expect(screen.getByRole('button', { name: 'Verify' })).toBeTruthy()
  })

  it('returns to the password when the challenge is dead', async () => {
    // Too many wrong codes, or too long looking for the phone: there is
    // nothing left to retry against, so retrying the code would be a trap.
    apiPost
      .mockResolvedValueOnce(CHALLENGE)
      .mockRejectedValueOnce(new ApiError(401, 'auth.mfa_challenge_expired', 'gone'))
    const user = userEvent.setup()
    renderLogin()

    await signIn(user)
    await screen.findByText('Two-step verification')
    await user.type(screen.getByLabelText('Code'), '000000')
    await user.click(screen.getByRole('button', { name: 'Verify' }))

    await screen.findByText(/Sign in again/)
    expect(screen.getByRole('button', { name: 'Sign in' })).toBeTruthy()
  })

  it('does not ask for a code when no factor is enrolled', async () => {
    apiPost.mockResolvedValueOnce(TOKEN)
    const user = userEvent.setup()
    renderLogin()

    await signIn(user)

    await waitFor(() => expect(apiGet).toHaveBeenCalledWith('/auth/me'))
    expect(screen.queryByText('Two-step verification')).toBeNull()
  })
})

describe('enrolling a second factor', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    apiGet.mockResolvedValue(USER)
    // Signed in, so the panel sees an account rather than nobody.
    refreshSession.mockResolvedValue(true)
  })

  function renderPanel() {
    return render(
      <AuthProvider>
        <TwoFactorPanel />
      </AuthProvider>,
    )
  }

  it('shows a QR code and the typed key, then confirms with a code', async () => {
    apiPost.mockResolvedValueOnce({
      secret: 'JBSWY3DPEHPK3PXP',
      otpauth_url: 'otpauth://totp/ProxUI:jsmith?secret=JBSWY3DPEHPK3PXP',
      digits: 6,
      period: 30,
    })
    const user = userEvent.setup()
    renderPanel()

    await user.click(screen.getByRole('button', { name: /Set up two-step/ }))
    await waitFor(() => expect(apiPost).toHaveBeenCalledWith('/auth/me/totp', {}))

    // Both routes in: the QR for a phone, the key for anything that cannot
    // scan one.
    await screen.findByAltText(/QR code/)
    expect(screen.getByText('JBSWY3DPEHPK3PXP')).toBeTruthy()

    apiPost.mockResolvedValueOnce(undefined)
    await user.type(screen.getByLabelText('Code from the app'), '123456')
    await user.click(screen.getByRole('button', { name: 'Turn on' }))

    await waitFor(() =>
      expect(apiPost).toHaveBeenCalledWith('/auth/me/totp/confirm', { code: '123456' }),
    )
  })

  it('will not remove the factor without a password', async () => {
    apiGet.mockResolvedValue({ ...USER, totp_enabled: true })
    const user = userEvent.setup()
    renderPanel()

    await user.click(await screen.findByRole('button', { name: /Remove authenticator/ }))
    // The confirm button stays disabled until a password is typed.
    const remove = screen.getByRole('button', { name: 'Remove' }) as HTMLButtonElement
    expect(remove.disabled).toBe(true)

    apiDel.mockResolvedValueOnce(undefined)
    await user.type(screen.getByPlaceholderText('Your password'), 'correct horse')
    await user.click(remove)

    await waitFor(() =>
      expect(apiDel).toHaveBeenCalledWith('/auth/me/totp', { password: 'correct horse' }),
    )
  })
})
