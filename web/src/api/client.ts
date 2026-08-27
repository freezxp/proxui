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

/**
 * The message to show for a failed call: the server's own explanation when
 * there is one, and the caller's wording when there is not.
 *
 * Lives here beside ApiError because every catch block in the app wants the
 * same two lines, and the ones that wrote them by hand had already begun to
 * disagree about what a non-ApiError should say.
 */
export function detailOf(err: unknown, fallback: string): string {
  return err instanceof ApiError && err.detail ? err.detail : fallback
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

/** Fetches a file, carrying the same auth as any other call. Exports cannot be
 *  plain links: the access token lives in memory, so a browser navigation
 *  would arrive unauthenticated. */
export async function requestBlob(path: string): Promise<Blob> {
  const send = () =>
    fetch(`/api/v1${path}`, {
      credentials: 'same-origin',
      headers: accessToken ? { Authorization: `Bearer ${accessToken}` } : {},
    })

  let response = await send()
  if (response.status === 401 && (await refreshSession())) {
    response = await send()
  }
  if (!response.ok) throw await parseError(response)
  return response.blob()
}

/**
 * Uploads one file, reporting progress.
 *
 * XMLHttpRequest rather than fetch, for the one thing fetch still cannot do:
 * report how much of a request body has been sent. Moving a gigabyte to a
 * guest with no progress bar is indistinguishable from a hung portal.
 *
 * The body is the file itself rather than multipart form data — the server
 * takes the directory and name from the query string and streams the body
 * straight to the guest, so there is nothing to parse and nothing to buffer.
 */
export function uploadFile(
  path: string,
  file: File,
  onProgress?: (fraction: number) => void,
  signal?: AbortSignal,
): Promise<{ path: string; bytes: number }> {
  const send = () =>
    new Promise<{ status: number; body: string }>((resolve, reject) => {
      const xhr = new XMLHttpRequest()
      xhr.open('POST', `/api/v1${path}`)
      xhr.withCredentials = true
      if (accessToken) xhr.setRequestHeader('Authorization', `Bearer ${accessToken}`)
      xhr.setRequestHeader('Content-Type', 'application/octet-stream')

      if (onProgress) {
        xhr.upload.onprogress = (event) => {
          if (event.lengthComputable) onProgress(event.loaded / event.total)
        }
      }
      xhr.onload = () => resolve({ status: xhr.status, body: xhr.responseText })
      xhr.onerror = () => reject(new ApiError(0, 'network', 'The upload could not be sent.'))
      xhr.onabort = () => reject(new ApiError(0, 'aborted', 'The upload was cancelled.'))
      signal?.addEventListener('abort', () => xhr.abort(), { once: true })
      xhr.send(file)
    })

  const finish = async (result: { status: number; body: string }) => {
    let parsed: Record<string, unknown> = {}
    try {
      parsed = JSON.parse(result.body)
    } catch {
      /* a proxy error page, or an empty body */
    }
    if (result.status < 200 || result.status >= 300) {
      throw new ApiError(
        result.status,
        (parsed.code as string) ?? 'unknown',
        (parsed.detail as string) ?? 'The upload failed.',
        parsed.request_id as string,
        parsed.fields as Record<string, string>,
        parsed,
      )
    }
    return parsed as unknown as { path: string; bytes: number }
  }

  return send().then(async (result) => {
    // One retry after a refresh, matching request(): a long upload can outlive
    // the access token that started it.
    if (result.status === 401 && (await refreshSession())) return finish(await send())
    return finish(result)
  })
}

/**
 * Releases a resource as the page goes away.
 *
 * `keepalive` is what lets the request outlive the document that started it.
 * `navigator.sendBeacon` is the usual tool for this and cannot be used here:
 * it sets no headers, and this API authenticates with a bearer token held in
 * memory rather than with a cookie.
 *
 * Best-effort by construction. There is no waiting for the response and no
 * refresh-and-retry, because during an unload there is no time for either — a
 * token that expired mid-session means this call is simply lost. Every caller
 * must therefore have a server-side backstop; nothing may depend on it
 * arriving.
 */
export function releaseOnUnload(path: string): void {
  try {
    void fetch(`/api/v1${path}`, {
      method: 'DELETE',
      credentials: 'same-origin',
      keepalive: true,
      headers: accessToken ? { Authorization: `Bearer ${accessToken}` } : {},
    }).catch(() => {
      /* the page is going away; there is nobody left to tell */
    })
  } catch {
    /* a browser without keepalive refuses synchronously */
  }
}

export const api = {
  get: <T>(path: string) => request<T>(path),
  post: <T>(path: string, body?: unknown, options: RequestOptions = {}) =>
    request<T>(path, { ...options, method: 'POST', body: body ? JSON.stringify(body) : undefined }),
  put: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'PUT', body: body ? JSON.stringify(body) : undefined }),
  // A body on DELETE is unusual but not exotic, and it is what removing a
  // second factor needs: the password that authorizes it must not travel in a
  // query string, where it would land in logs and history.
  del: <T>(path: string, body?: unknown) =>
    request<T>(path, { method: 'DELETE', body: body ? JSON.stringify(body) : undefined }),
}
