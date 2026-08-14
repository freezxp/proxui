import { api } from '@/api/client'
import type { Role } from '@/api/types'

export interface LiveEvent {
  id: number
  occurred_at: string
  category: string
  type: string
  severity: 'info' | 'warning' | 'critical'
  payload: Record<string, unknown>
}

/**
 * Subscribes to the portal's live event stream.
 *
 * Each connection starts by asking for a single-use ticket, because a browser
 * cannot put an Authorization header on a WebSocket — the same reason the
 * console works this way. The access token is never put in the URL, where it
 * would survive in history and in every proxy log; a ticket that dies on first
 * use is worth much less if it leaks.
 *
 * Reconnects with backoff, because a dropped socket is normal (a laptop
 * sleeping, a proxy timing out) and the UI should recover without a reload.
 * The server scopes what it sends, so anything arriving here is already
 * something this user may see.
 */
export function subscribeToEvents(
  onEvent: (event: LiveEvent) => void,
  onStatus?: (connected: boolean) => void,
): () => void {
  let socket: WebSocket | null = null
  let closed = false
  let attempt = 0
  let timer: number | undefined

  const retryLater = () => {
    if (closed) return
    // Backoff caps at 30s: reconnecting every second through an outage would
    // add load exactly when the portal is least able to take it.
    const delay = Math.min(1000 * 2 ** attempt, 30_000)
    attempt++
    timer = window.setTimeout(() => void connect(), delay)
  }

  const connect = async () => {
    if (closed) return

    let wsPath: string
    try {
      const ticket = await api.post<{ ws_url: string }>('/events/ticket', {})
      wsPath = ticket.ws_url
    } catch {
      // No ticket means no stream. The portal still works; it just stops
      // updating on its own, so this retries rather than giving up.
      onStatus?.(false)
      retryLater()
      return
    }
    if (closed) return

    const protocol = window.location.protocol === 'https:' ? 'wss' : 'ws'
    socket = new WebSocket(`${protocol}://${window.location.host}${wsPath}`)

    socket.onopen = () => {
      attempt = 0
      onStatus?.(true)
    }
    socket.onmessage = (message) => {
      try {
        onEvent(JSON.parse(message.data) as LiveEvent)
      } catch {
        // A malformed frame must not kill the stream.
      }
    }
    socket.onclose = () => {
      onStatus?.(false)
      retryLater()
    }
    socket.onerror = () => socket?.close()
  }

  void connect()

  return () => {
    closed = true
    if (timer) window.clearTimeout(timer)
    socket?.close()
  }
}

/** Events every role may act on locally; others are ignored by the list. */
export function isVMEvent(event: LiveEvent): boolean {
  return event.type.startsWith('vm.')
}

export function eventVMId(event: LiveEvent): string | null {
  const id = event.payload['vm_id']
  return typeof id === 'string' ? id : null
}

export function canSubscribe(_role: Role): boolean {
  return true
}
