import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { AppRoutes } from '@/app/router'
import { AuthProvider } from '@/features/auth/useAuth'
import { applyTheme, storedTheme } from '@/lib/theme'
import './styles.css'

// Applied before the first paint so a dark-theme user never sees a white flash.
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
