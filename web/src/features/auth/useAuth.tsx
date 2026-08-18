import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { api, setAccessToken, setUnauthenticatedHandler, refreshSession } from '@/api/client'
import type { CurrentUser, MFAChallengeResponse, TokenResponse } from '@/api/types'

interface AuthState {
  user: CurrentUser | null
  /** True until the first refresh attempt settles, so the router does not
   *  bounce a returning user to the login page before their cookie is tried. */
  loading: boolean
  /** Signs in. Returns a challenge when the account has a second factor
   *  enrolled, in which case no session exists yet and `verifyMFA` finishes
   *  the job (AUTH-04). */
  login: (username: string, password: string) => Promise<MFAChallengeResponse | null>
  /** Completes a challenged sign-in with a code from the authenticator app. */
  verifyMFA: (mfaToken: string, code: string) => Promise<void>
  registerAccount: (input: RegisterInput) => Promise<void>
  logout: () => Promise<void>
  reload: () => Promise<void>
}

export interface RegisterInput {
  username: string
  email: string
  display_name: string
  password: string
}

const AuthContext = createContext<AuthState | null>(null)

export function AuthProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<CurrentUser | null>(null)
  const [loading, setLoading] = useState(true)

  const loadUser = useCallback(async () => {
    try {
      setUser(await api.get<CurrentUser>('/auth/me'))
    } catch {
      setUser(null)
    }
  }, [])

  useEffect(() => {
    setUnauthenticatedHandler(() => setUser(null))

    // A page reload loses the in-memory access token but keeps the refresh
    // cookie, so the session is restored rather than the user being asked to
    // log in again after every refresh.
    void (async () => {
      if (await refreshSession()) await loadUser()
      setLoading(false)
    })()
  }, [loadUser])

  const login = useCallback(
    async (username: string, password: string) => {
      const result = await api.post<TokenResponse | MFAChallengeResponse>(
        '/auth/login',
        { username, password },
        { skipRefresh: true },
      )
      // A challenge is a 200 with no token: the password was right and the
      // sign-in is half done. Nothing is stored, and the caller shows the code
      // form rather than treating it as a failure.
      if ('mfa_required' in result) return result
      setAccessToken(result.access_token)
      await loadUser()
      return null
    },
    [loadUser],
  )

  const verifyMFA = useCallback(
    async (mfaToken: string, code: string) => {
      const token = await api.post<TokenResponse>(
        '/auth/mfa',
        { mfa_token: mfaToken, code },
        { skipRefresh: true },
      )
      setAccessToken(token.access_token)
      await loadUser()
    },
    [loadUser],
  )

  // Registration signs the new account in straight away: the server issues a
  // session with the response, so a second form asking for the credentials
  // just chosen would be pure friction.
  const registerAccount = useCallback(
    async (input: RegisterInput) => {
      const token = await api.post<TokenResponse>('/auth/register', input, { skipRefresh: true })
      setAccessToken(token.access_token)
      await loadUser()
    },
    [loadUser],
  )

  const logout = useCallback(async () => {
    try {
      await api.post('/auth/logout', undefined, { skipRefresh: true })
    } finally {
      setAccessToken(null)
      setUser(null)
    }
  }, [])

  const value = useMemo<AuthState>(
    () => ({ user, loading, login, verifyMFA, registerAccount, logout, reload: loadUser }),
    [user, loading, login, verifyMFA, registerAccount, logout, loadUser],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used inside AuthProvider')
  return ctx
}
