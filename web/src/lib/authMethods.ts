import { useQuery } from '@tanstack/react-query'

export interface AuthMethods {
  password: boolean
  registration: boolean
  google: boolean
}

const DEFAULTS: AuthMethods = { password: true, registration: false, google: false }

/** Which ways in this portal offers. Fetched unauthenticated, like branding,
 *  because the sign-in page has to decide what to show before anyone has
 *  signed in. Defaults to password-only: if this cannot be read, offering a
 *  door that does not exist is worse than not offering one. */
export function useAuthMethods(): AuthMethods {
  const query = useQuery({
    queryKey: ['auth-methods'],
    queryFn: async (): Promise<AuthMethods> => {
      const response = await fetch('/api/v1/auth/methods', { credentials: 'same-origin' })
      if (!response.ok) return DEFAULTS
      return { ...DEFAULTS, ...(await response.json()) }
    },
    staleTime: 60_000,
    retry: false,
  })
  return query.data ?? DEFAULTS
}

/** Why an external sign-in did not finish. The callback redirects here with a
 *  short reason rather than rendering its own page, so the person lands back
 *  where they started. */
export const SSO_MESSAGES: Record<string, string> = {
  cancelled: 'Sign-in was cancelled.',
  expired: 'That sign-in took too long. Please try again.',
  no_account:
    'There is no account for that address, and this portal is not accepting new registrations. Ask an administrator.',
  inactive: 'That account is disabled. Ask an administrator.',
  unavailable: 'Google sign-in is not configured on this portal.',
  rejected: 'Google could not confirm that sign-in.',
}
