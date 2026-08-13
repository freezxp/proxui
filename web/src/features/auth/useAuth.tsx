import { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react'
import type { ReactNode } from 'react'
import { api, setAccessToken, setUnauthenticatedHandler, refreshSession } from '@/api/client'
import type { CurrentUser, TokenResponse } from '@/api/types'

interface AuthState {
  user: CurrentUser | null
  /** True until the first refresh attempt settles, so the router does not
   *  bounce a returning user to the login page before their cookie is tried. */
  loading: boolean
  login: (username: string, password: string) => Promise<void>
  logout: () => Promise<void>
  reload: () => Promise<void>
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
      const token = await api.post<TokenResponse>(
        '/auth/login',
        { username, password },
        { skipRefresh: true },
      )
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
    () => ({ user, loading, login, logout, reload: loadUser }),
    [user, loading, login, logout, loadUser],
  )

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>
}

export function useAuth(): AuthState {
  const ctx = useContext(AuthContext)
  if (!ctx) throw new Error('useAuth must be used inside AuthProvider')
  return ctx
}
