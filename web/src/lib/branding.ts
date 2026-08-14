import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'

export interface Branding {
  'branding.portal_name': string
  'branding.logo': string
  'branding.login_banner': string
}

const DEFAULTS: Branding = {
  'branding.portal_name': 'ProxUI',
  'branding.logo': '',
  'branding.login_banner': '',
}

/** Branding is fetched unauthenticated, because the sign-in page needs it
 *  before anyone has signed in. It also sets the browser tab, so a portal
 *  someone renamed is recognisable among twenty other tabs. */
export function useBranding(): Branding {
  const query = useQuery({
    queryKey: ['branding'],
    queryFn: async (): Promise<Branding> => {
      // Deliberately not the API client: this runs before authentication and
      // must not trigger a token refresh or a redirect to the login page.
      const response = await fetch('/api/v1/branding', { credentials: 'same-origin' })
      if (!response.ok) return DEFAULTS
      return { ...DEFAULTS, ...(await response.json()) }
    },
    staleTime: 60_000,
    // A portal that will not render because its logo could not be fetched
    // would be a poor trade.
    retry: false,
  })

  const branding = query.data ?? DEFAULTS
  const name = branding['branding.portal_name'] || DEFAULTS['branding.portal_name']

  useEffect(() => {
    document.title = name
  }, [name])

  return { ...branding, 'branding.portal_name': name }
}
