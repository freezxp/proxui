/**
 * The API client.
 *
 * Two things here are load-bearing. The access token is held in memory only:
 * localStorage would expose it to any script that runs on the page, and the
 * refresh cookie already survives a reload. And a 401 triggers exactly one
 * refresh attempt, with concurrent callers waiting on the same promise, so a
 * dashboard firing six requests at once cannot start six refreshes and trip
 * the server's reuse detection.
 */

export class ApiError extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly detail: string,
    readonly requestId?: string,
    readonly fields?: Record<string, string>,
    // The raw body, for the few endpoints whose failure carries a useful
    // payload rather than only a message — the platform connection test
    // returns how far it got alongside the reason it stopped.
    readonly body?: unknown,
  ) {
    super(detail || code)
    this.name = 'ApiError'
  }
}

let accessToken: string | null = null
let refreshing: Promise<boolean> | null = null
let onUnauthenticated: (() => void) | null = null

export function setAccessToken(token: string | null): void {
  accessToken = token
}

export function hasAccessToken(): boolean {
  return accessToken !== null
}

/** Registers what to do when refreshing fails: normally, show the login page. */
export function setUnauthenticatedHandler(fn: () => void): void {
  onUnauthenticated = fn
}

async function parseError(response: Response): Promise<ApiError> {
  try {
    const body = await response.json()
    return new ApiError(
      response.status,
      body.code ?? 'unknown',
      body.detail ?? body.error ?? response.statusText,
      body.request_id,
      body.fields,
      body,
    )
  } catch {
    return new ApiError(response.status, 'unknown', response.statusText)
  }
}

/** Exchanges the refresh cookie for a new access token. */
export async function refreshSession(): Promise<boolean> {
  if (refreshing) return refreshing

  refreshing = (async () => {
    try {
      const response = await fetch('/api/v1/auth/refresh', {
        method: 'POST',
        credentials: 'same-origin',
      })
      if (!response.ok) return false
      const body = await response.json()
      accessToken = body.access_token
      return true
    } catch {
      return false
    } finally {
      refreshing = null
    }
  })()

  return refreshing
}

interface RequestOptions extends RequestInit {
  /** Set for the auth endpoints, which must not recurse into a refresh. */
  skipRefresh?: boolean
}

export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { skipRefresh, ...init } = options

  const send = () =>
    fetch(`/api/v1${path}`, {
      ...init,
      credentials: 'same-origin',
      headers: {
        ...(init.body ? { 'Content-Type': 'application/json' } : {}),
        ...(accessToken ? { Authorization: `Bearer ${accessToken}` } : {}),
        ...init.headers,
      },
    })

  let response = await send()

  if (response.status === 401 && !skipRefresh) {
    if (await refreshSession()) {
      response = await send()
    } else {
      accessToken = null
      onUnauthenticated?.()
      throw await parseError(response)
    }
  }

  if (!response.ok) throw await parseError(response)
  if (response.status === 204) return undefined as T
  return (await response.json()) as T
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown, options: RequestOptions = {}) =>
    request<T>(path, { ...options, method: 'POST', body: body ? JSON.stringify(body) : undefined }),
  put: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'PUT', body: body ? JSON.stringify(body) : undefined }),
  del: <T>(path: string) => request<T>(path, { method: 'DELETE' }),
}
