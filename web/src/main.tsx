import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { AppRoutes } from '@/app/router'
import { AuthProvider } from '@/features/auth/useAuth'
import { applyMode, applyTheme, storedMode, storedTheme } from '@/lib/theme'
// IBM Plex, bundled rather than fetched: a self-hosted portal should not need
// a font CDN to render, and a stylesheet pulled from Google on every load would
// both leak who is using it and leave an air-gapped install in a fallback face.
//
// Six weights, Latin only — the whole family's subsets are 68 files and a
// megabyte, against six and 140 kB for these. The portal's own text is English;
// a guest named in Cyrillic or Japanese renders in the system font behind Plex,
// which is what a fallback stack is for.
import '@fontsource/ibm-plex-sans/latin-400.css'
import '@fontsource/ibm-plex-sans/latin-500.css'
import '@fontsource/ibm-plex-sans/latin-600.css'
import '@fontsource/ibm-plex-sans/latin-700.css'
import '@fontsource/ibm-plex-mono/latin-400.css'
import '@fontsource/ibm-plex-mono/latin-500.css'
import './styles.css'

// Applied before the first paint so a dark-mode user never sees a white flash,
// and nobody sees the wrong palette resolve into the right one.
applyMode(storedMode())
applyTheme(storedTheme())

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 10_000,
      // The client already retries a 401 once after refreshing; retrying
      // beyond that turns one rejected request into several.
      retry: 1,
      refetchOnWindowFocus: false,
    },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <AuthProvider>
        <AppRoutes />
      </AuthProvider>
    </QueryClientProvider>
  </StrictMode>,
)
