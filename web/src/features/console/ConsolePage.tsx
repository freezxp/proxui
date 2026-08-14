import { useCallback, useEffect, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import RFB from '@novnc/novnc'
import { api, ApiError } from '@/api/client'
import type { VMDetail } from '@/api/types'
import { StateBadge } from '@/components/StateBadge'

type Phase = 'connecting' | 'connected' | 'disconnected' | 'error'

type Session = { session_id: string; ws_url: string; expires_in: number }

// The bridge sends these on close; anything else is an unexpected drop
// (docs/08-api-specification.md §8.4).
const CLOSE_REASONS: Record<number, string> = {
  4000: 'The console closed after a period without activity.',
  4001: 'The console reached its maximum session length.',
  4002: 'An administrator ended this console session.',
  4003: 'The platform closed the console connection.',
  4004: 'The platform console is unavailable. The VM may be stopped, or the node unreachable.',
}

export function ConsolePage() {
  const { vmId = '' } = useParams()
  const screenRef = useRef<HTMLDivElement>(null)
  const rfbRef = useRef<RFB | null>(null)
  const [phase, setPhase] = useState<Phase>('connecting')
  const [message, setMessage] = useState('')
  const [vm, setVM] = useState<VMDetail | null>(null)
  const [attempt, setAttempt] = useState(0)

  // The clipboard panel. Text is moved through a textarea rather than synced
  // silently with the local clipboard, because reading the local clipboard
  // needs navigator.clipboard.readText — a permission prompt, and one that
  // does not exist at all outside a secure context, which the plain-HTTP LAN
  // deployment is. Ctrl+V into a textarea needs neither and works everywhere.
  const [clipboardOpen, setClipboardOpen] = useState(false)
  const [outgoing, setOutgoing] = useState('')
  const [incoming, setIncoming] = useState('')
  const [notice, setNotice] = useState('')
  const incomingRef = useRef<HTMLTextAreaElement>(null)
  const noticeTimer = useRef<number | undefined>(undefined)

  const flash = useCallback((text: string) => {
    setNotice(text)
    window.clearTimeout(noticeTimer.current)
    noticeTimer.current = window.setTimeout(() => setNotice(''), 2000)
  }, [])
  useEffect(() => () => window.clearTimeout(noticeTimer.current), [])

  useEffect(() => {
    api
      .get<VMDetail>(`/vms/${vmId}`)
      .then(setVM)
      .catch(() => undefined)
  }, [vmId])

  useEffect(() => {
    let cancelled = false
    let rfb: RFB | null = null
    let socket: WebSocket | null = null
    // Whether RFB ever reached a usable session. It separates "the console
    // ended" from "the console never started", which need different messages.
    let established = false

    async function connect() {
      setPhase('connecting')
      setMessage('')
      try {
        // The ticket is single-use and short-lived, so it is fetched per
        // connection attempt rather than reused on reconnect.
        const session = await api.post<Session>(`/vms/${vmId}/console`, {
          kind: 'vnc',
        })
        if (cancelled || !screenRef.current) return

        const url = new URL(session.ws_url, window.location.href)
        url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'

        // The socket is opened here rather than by noVNC so that the bridge's
        // close codes are readable: noVNC's own disconnect event reports only
        // whether the close was clean, which would reduce "idle timeout",
        // "admin ended it" and "node unreachable" to one blank message.
        socket = new WebSocket(url.toString(), 'binary')
        socket.binaryType = 'arraybuffer'
        socket.addEventListener('close', (event) => {
          if (cancelled) return
          const known = CLOSE_REASONS[event.code]
          if (known) {
            setMessage(known)
            setPhase(event.code === 4003 || event.code === 4004 ? 'error' : 'disconnected')
            return
          }
          // Every close has to end the spinner. A socket that dies before RFB
          // starts — a proxy in the way, a failed subprotocol negotiation —
          // produces no noVNC event at all, and a page that spins forever
          // tells the operator nothing about what to fix.
          if (established) {
            setPhase('disconnected')
            setMessage('The console session ended.')
            return
          }
          setPhase('error')
          setMessage(
            `The console connection closed before it was established (code ${event.code}). ` +
              'Check that nothing between this browser and the portal is filtering WebSocket traffic.',
          )
        })

        rfb = new RFB(screenRef.current, socket, {
          // No credentials: the portal answered the platform's console
          // challenge server-side, so the browser holds no secret (ADR 0002).
          // That is also why this works over plain HTTP on a LAN — with
          // security type "None" noVNC never reaches for WebCrypto, which is
          // unavailable outside a secure context.
          wsProtocols: ['binary'],
        })
        rfb.scaleViewport = true
        rfb.clipViewport = true
        rfb.background = 'transparent'
        rfbRef.current = rfb

        rfb.addEventListener('connect', () => {
          if (cancelled) return
          established = true
          setPhase('connected')
        })
        rfb.addEventListener('disconnect', ((event: CustomEvent<{ clean: boolean }>) => {
          if (cancelled) return
          // A close code, if the bridge sent one, already produced a better
          // message than anything available here.
          setPhase((current) =>
            current === 'connected' ? (event.detail?.clean ? 'disconnected' : 'error') : current,
          )
          setMessage(
            (current) =>
              current ||
              (event.detail?.clean
                ? 'The console session ended.'
                : 'The console connection dropped.'),
          )
        }) as EventListener)
        rfb.addEventListener('securityfailure', () => {
          if (cancelled) return
          setPhase('error')
          setMessage('The platform rejected the console session.')
        })
        // The guest copied something. RFB pushes cut text unasked, so this
        // arrives whether or not the panel is open — opening it later still
        // finds the last thing copied rather than nothing.
        rfb.addEventListener('clipboard', ((event: CustomEvent<{ text: string }>) => {
          if (cancelled || typeof event.detail?.text !== 'string') return
          setIncoming(event.detail.text)
        }) as EventListener)
      } catch (err) {
        if (cancelled) return
        setPhase('error')
        setMessage(consoleError(err))
      }
    }

    void connect()
    return () => {
      cancelled = true
      rfb?.disconnect()
      socket?.close()
      rfbRef.current = null
    }
  }, [vmId, attempt])

  const send = useCallback((keys: () => void) => () => keys(), [])
  const ctrlAltDel = send(() => rfbRef.current?.sendCtrlAltDel())

  // Sending puts the text on the *guest's* clipboard; pasting it is still a
  // Ctrl+V inside the guest. Saying so in the panel saves the "I clicked send
  // and nothing appeared" round trip.
  const sendClipboard = useCallback(() => {
    const rfb = rfbRef.current
    if (!rfb || outgoing === '') return
    rfb.clipboardPasteFrom(outgoing)
    flash("Sent. Paste inside the VM with the guest's own paste shortcut.")
  }, [outgoing, flash])

  const copyIncoming = useCallback(async () => {
    if (incoming === '') return
    try {
      await navigator.clipboard.writeText(incoming)
      flash('Copied to this computer’s clipboard.')
    } catch {
      // No permission, or no secure context. Selecting the text leaves the
      // user one Ctrl+C away rather than at a dead end.
      incomingRef.current?.select()
      flash('Press Ctrl+C to copy the selected text.')
    }
  }, [incoming, flash])

  const [fullscreen, setFullscreen] = useState(false)
  useEffect(() => {
    const onChange = () => setFullscreen(Boolean(document.fullscreenElement))
    document.addEventListener('fullscreenchange', onChange)
    return () => document.removeEventListener('fullscreenchange', onChange)
  }, [])

  return (
    <div className="flex h-screen flex-col bg-black">
      <header className="flex items-center gap-3 border-b border-border bg-surface px-4 py-2">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate font-medium">{vm?.name ?? 'Console'}</span>
          {vm && <StateBadge state={vm.state} />}
        </div>

        <ConnectionDot phase={phase} />

        <div className="ml-auto flex items-center gap-2">
          <ToolbarButton onClick={ctrlAltDel} disabled={phase !== 'connected'}>
            Ctrl+Alt+Del
          </ToolbarButton>
          <ToolbarButton
            onClick={() => setClipboardOpen((open) => !open)}
            pressed={clipboardOpen}
            disabled={phase !== 'connected'}
          >
            Clipboard
          </ToolbarButton>
          <ToolbarButton
            onClick={() => {
              if (document.fullscreenElement) void document.exitFullscreen()
              else void document.documentElement.requestFullscreen()
            }}
          >
            {fullscreen ? 'Exit fullscreen' : 'Fullscreen'}
          </ToolbarButton>
          {phase === 'connected' ? (
            <ToolbarButton onClick={() => rfbRef.current?.disconnect()}>Disconnect</ToolbarButton>
          ) : (
            <ToolbarButton onClick={() => setAttempt((n) => n + 1)} primary>
              Reconnect
            </ToolbarButton>
          )}
          <ToolbarButton onClick={() => window.close()}>Close</ToolbarButton>
        </div>
      </header>

      <div className="relative flex min-h-0 flex-1">
        <div className="relative min-w-0 flex-1 overflow-hidden">
          <div ref={screenRef} className="h-full w-full" />

          {phase !== 'connected' && (
            <div className="absolute inset-0 flex items-center justify-center bg-black/80 p-6">
              <div className="max-w-md space-y-3 text-center">
                {phase === 'connecting' && (
                  <>
                    <div className="mx-auto h-6 w-6 animate-spin rounded-full border-2 border-white/30 border-t-white" />
                    <p className="text-sm text-white/80">Connecting to the console…</p>
                  </>
                )}
                {phase !== 'connecting' && (
                  <>
                    <p
                      className={`text-sm ${phase === 'error' ? 'text-red-400' : 'text-white/80'}`}
                    >
                      {message}
                    </p>
                    <button
                      onClick={() => setAttempt((n) => n + 1)}
                      className="rounded-md bg-accent px-4 py-2 text-sm font-medium text-white"
                    >
                      Reconnect
                    </button>
                  </>
                )}
              </div>
            </div>
          )}
        </div>

        {clipboardOpen && (
          <aside
            aria-label="Clipboard"
            className="flex w-80 shrink-0 flex-col gap-4 overflow-y-auto border-l border-border bg-surface p-4"
          >
            <div className="space-y-2">
              <label htmlFor="clipboard-out" className="block text-sm font-medium">
                Send to the VM
              </label>
              <p className="text-xs text-muted">
                Paste here, then send. It lands on the VM&apos;s clipboard — paste it inside the VM
                as you normally would.
              </p>
              <textarea
                id="clipboard-out"
                value={outgoing}
                onChange={(event) => setOutgoing(event.target.value)}
                rows={5}
                spellCheck={false}
                className="w-full rounded-md border border-border bg-surface-raised p-2 font-mono text-xs"
              />
              <div className="flex gap-2">
                <ToolbarButton
                  onClick={sendClipboard}
                  disabled={phase !== 'connected' || outgoing === ''}
                  primary
                >
                  Send
                </ToolbarButton>
                <ToolbarButton onClick={() => setOutgoing('')} disabled={outgoing === ''}>
                  Clear
                </ToolbarButton>
              </div>
            </div>

            <div className="space-y-2 border-t border-border pt-4">
              <label htmlFor="clipboard-in" className="block text-sm font-medium">
                Copied in the VM
              </label>
              <p className="text-xs text-muted">
                {incoming === ''
                  ? 'Copy something inside the VM and it appears here.'
                  : 'Copy this to your own clipboard.'}
              </p>
              <textarea
                id="clipboard-in"
                ref={incomingRef}
                value={incoming}
                readOnly
                rows={5}
                spellCheck={false}
                className="w-full rounded-md border border-border bg-surface-raised p-2 font-mono text-xs"
              />
              <ToolbarButton onClick={() => void copyIncoming()} disabled={incoming === ''}>
                Copy
              </ToolbarButton>
            </div>

            {/* Announced politely: the send and copy buttons give no other
                confirmation, and a silent success reads as a broken button. */}
            <p aria-live="polite" className="min-h-[2rem] text-xs text-muted">
              {notice}
            </p>
          </aside>
        )}
      </div>
    </div>
  )
}

function ConnectionDot({ phase }: { phase: Phase }) {
  const colour =
    phase === 'connected'
      ? 'bg-state-running'
      : phase === 'connecting'
        ? 'bg-state-paused'
        : 'bg-state-stopped'
  return (
    <span className="flex items-center gap-1.5 text-xs text-muted">
      <span className={`h-2 w-2 rounded-full ${colour}`} />
      {phase}
    </span>
  )
}

function ToolbarButton({
  children,
  onClick,
  disabled,
  primary,
  pressed,
}: {
  children: React.ReactNode
  onClick: () => void
  disabled?: boolean
  primary?: boolean
  pressed?: boolean
}) {
  return (
    <button
      onClick={onClick}
      disabled={disabled}
      aria-pressed={pressed}
      className={`rounded-md px-3 py-1.5 text-sm disabled:opacity-40 ${
        primary
          ? 'bg-accent text-white'
          : pressed
            ? 'border border-accent bg-accent/10 text-accent'
            : 'border border-border hover:bg-surface-raised'
      }`}
    >
      {children}
    </button>
  )
}

// Console failures have specific causes an operator can act on, so they are
// named rather than collapsed into "something went wrong".
function consoleError(err: unknown): string {
  if (err instanceof ApiError) {
    if (err.status === 403) return 'Your account is not permitted to open a console on this VM.'
    if (err.status === 404) return 'This VM does not exist, or is not visible to your account.'
    if (err.status === 429) return 'Too many console requests. Wait a moment and try again.'
    if (err.status === 409) return 'This VM is not running, so it has no console.'
    return err.message
  }
  return 'Could not open a console session.'
}
