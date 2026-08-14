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

  const connect = () => {
    if (closed) return
    const protocol = window.location.protocol === 'https:' ? 'wss' : 'ws'
    socket = new WebSocket(`${protocol}://${window.location.host}/api/v1/events`)

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
      if (closed) return
      // Backoff caps at 30s: reconnecting every second through an outage
      // would add load exactly when the portal is least able to take it.
      const delay = Math.min(1000 * 2 ** attempt, 30_000)
      attempt++
      timer = window.setTimeout(connect, delay)
    }
    socket.onerror = () => socket?.close()
  }

  connect()

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
