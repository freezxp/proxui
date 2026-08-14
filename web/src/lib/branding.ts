import { useEffect } from 'react'
import { useQuery } from '@tanstack/react-query'

export interface Branding {
  'branding.portal_name': string
  'branding.logo': string
  'branding.login_banner': string
}

const STORED_DEFAULTS: Branding = {
  'branding.portal_name': '',
  'branding.logo': '',
  'branding.login_banner': '',
}

/** The name shown when nothing else can be worked out. Reached only if the
 *  page has no hostname at all, which means it is not being served over
 *  HTTP — a file:// open, or a test. */
const FALLBACK_NAME = 'ProxUI'

/**
 * Resolves what to call the portal.
 *
 * An unset name follows the address the browser used, which for a self-hosted
 * tool is usually the best name it could have: whoever reaches it at
 * `vm.example.com` already thinks of it by that name. Resolved here rather
 * than on the server because the server sees whatever `Host` a reverse proxy
 * chose to pass along, while the browser knows what the person actually typed.
 *
 * The port is dropped. "exstudios.vm:8080" is an address; "exstudios.vm" is a
 * name.
 */
export function resolvePortalName(configured: string, hostname: string): string {
  const chosen = configured.trim()
  if (chosen) return chosen
  return hostname.trim() || FALLBACK_NAME
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
      if (!response.ok) return STORED_DEFAULTS
      return { ...STORED_DEFAULTS, ...(await response.json()) }
    },
    staleTime: 60_000,
    // A portal that will not render because its logo could not be fetched
    // would be a poor trade.
    retry: false,
  })

  const branding = query.data ?? STORED_DEFAULTS
  const name = resolvePortalName(branding['branding.portal_name'], window.location.hostname)

  useEffect(() => {
    document.title = name
  }, [name])

  return { ...branding, 'branding.portal_name': name }
}
