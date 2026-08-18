import { useCallback, useEffect, useRef, useState } from 'react'
import { useParams } from 'react-router-dom'
import RFB from '@novnc/novnc'
import { api, ApiError } from '@/api/client'
import type { VMDetail } from '@/api/types'
import { StateBadge } from '@/components/StateBadge'
import { useViewportHeight } from '@/lib/viewport'

type Phase = 'connecting' | 'connected' | 'disconnected' | 'error'

type Session = { session_id: string; ws_url: string; expires_in: number }

// X keysyms for the keys a phone keyboard either lacks or swallows. noVNC has
// these in core/input/keysym.js, but the package's export map exposes only its
// root, so importing them directly would not survive a build.
const XK_BackSpace = 0xff08
const XK_Tab = 0xff09
const XK_Return = 0xff0d
const XK_Escape = 0xff1b
const XK_Left = 0xff51
const XK_Up = 0xff52
const XK_Right = 0xff53
const XK_Down = 0xff54

/**
 * The keysym for a character.
 *
 * Latin-1 maps one to one, and everything else goes through the Unicode
 * plane — the same two rules noVNC's own lookup starts with. Its third rule is
 * a table of legacy mappings for servers that predate Unicode keysyms, which
 * is both large and unnecessary here.
 */
function keysymFor(codepoint: number): number {
  if (codepoint >= 0x20 && codepoint <= 0xff) return codepoint
  return 0x01000000 | codepoint
}

// A phone keyboard reports almost nothing useful in keydown, so typing is
// recovered by diffing the value of a hidden field. The field starts full of
// filler so that a backspace at the start of a session has something to eat
// and still registers as a keystroke rather than as nothing happening.
const KEYBOARD_FILLER = '_'.repeat(100)

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

  // The soft keyboard. A touch device has no physical keys to capture, so a
  // hidden field is focused to summon the on-screen keyboard and what it types
  // is translated into RFB key events.
  const [keyboardOn, setKeyboardOn] = useState(false)
  const keyboardRef = useRef<HTMLTextAreaElement>(null)
  const lastInput = useRef(KEYBOARD_FILLER)

  const resetKeyboardField = useCallback(() => {
    const field = keyboardRef.current
    if (!field) return
    field.value = KEYBOARD_FILLER
    lastInput.current = KEYBOARD_FILLER
    // Put the caret at the end, so the first keystroke appends rather than
    // overwriting the filler and reading as a hundred backspaces.
    field.setSelectionRange(KEYBOARD_FILLER.length, KEYBOARD_FILLER.length)
  }, [])

  const toggleKeyboard = useCallback(() => {
    if (keyboardOn) {
      keyboardRef.current?.blur()
      setKeyboardOn(false)
      return
    }
    resetKeyboardField()
    // focus() must happen synchronously in the click handler: iOS only opens
    // the keyboard for a focus it can attribute to a user gesture.
    keyboardRef.current?.focus()
    setKeyboardOn(true)
  }, [keyboardOn, resetKeyboardField])

  const pressKey = useCallback((keysym: number, code: string) => {
    rfbRef.current?.sendKey(keysym, code)
    // Typing through the strip should not dismiss the keyboard.
    keyboardRef.current?.focus()
  }, [])

  /**
   * Turns an edit of the hidden field into key events.
   *
   * The field's value is compared with what it was: the shared prefix is
   * untouched, whatever the old value had beyond it was deleted, and whatever
   * the new value has beyond it was typed. Autocorrect and swipe typing
   * replace whole words at once, and this reproduces them as the backspaces
   * and characters a keyboard would have sent.
   */
  const onKeyboardInput = useCallback(() => {
    const rfb = rfbRef.current
    const field = keyboardRef.current
    if (!rfb || !field) return

    const next = field.value
    const previous = lastInput.current
    const newLen = Math.max(field.selectionStart ?? next.length, next.length)
    const oldLen = previous.length

    let typed = newLen - oldLen
    let deleted = typed < 0 ? -typed : 0
    for (let i = 0; i < Math.min(oldLen, newLen); i++) {
      if (next.charAt(i) !== previous.charAt(i)) {
        typed = newLen - i
        deleted = oldLen - i
        break
      }
    }

    for (let i = 0; i < deleted; i++) rfb.sendKey(XK_BackSpace, 'Backspace')
    for (let i = newLen - typed; i < newLen; i++) {
      const codepoint = next.codePointAt(i)
      if (codepoint !== undefined) rfb.sendKey(keysymFor(codepoint), null)
    }

    // The field would otherwise grow without bound over a long session.
    if (next.length > 2 * KEYBOARD_FILLER.length) resetKeyboardField()
    else lastInput.current = next
  }, [resetKeyboardField])

  // Enter and Tab have to be caught here: left alone they insert a newline or
  // move focus out of the field, and neither reaches the guest.
  const onKeyboardKeyDown = useCallback((event: React.KeyboardEvent<HTMLTextAreaElement>) => {
    const special: Record<string, [number, string]> = {
      Enter: [XK_Return, 'Enter'],
      Tab: [XK_Tab, 'Tab'],
      Escape: [XK_Escape, 'Escape'],
    }
    const match = special[event.key]
    if (!match) return
    event.preventDefault()
    rfbRef.current?.sendKey(match[0], match[1])
  }, [])

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
  const viewportHeight = useViewportHeight()
  useEffect(() => {
    const onChange = () => setFullscreen(Boolean(document.fullscreenElement))
    document.addEventListener('fullscreenchange', onChange)
    return () => document.removeEventListener('fullscreenchange', onChange)
  }, [])

  return (
    // 100dvh rather than 100vh: on a phone, 100vh is the height with the
    // browser's own bars hidden, so the bottom of the console sits underneath
    // them until the user scrolls. 100dvh still does not shrink when the soft
    // keyboard opens, which is precisely when the key strip below has to be
    // visible, so the measured visual viewport wins where it is available.
    <div
      className="flex h-[100dvh] flex-col bg-black"
      style={viewportHeight === null ? undefined : { height: viewportHeight }}
    >
      <header className="flex flex-wrap items-center gap-x-3 gap-y-2 border-b border-border bg-surface px-3 py-2 sm:px-4">
        <div className="flex min-w-0 items-center gap-2">
          <span className="truncate font-medium">{vm?.name ?? 'Console'}</span>
          {vm && <StateBadge state={vm.state} />}
        </div>

        <ConnectionDot phase={phase} />

        {/* Scrolls sideways rather than wrapping into three rows of buttons on
            a narrow screen, which would leave very little console. */}
        <div className="-mx-3 flex w-full items-center gap-2 overflow-x-auto px-3 sm:mx-0 sm:ml-auto sm:w-auto sm:overflow-visible sm:px-0">
          <ToolbarButton
            onClick={toggleKeyboard}
            pressed={keyboardOn}
            disabled={phase !== 'connected'}
          >
            Keyboard
          </ToolbarButton>
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
            // Full width over the console on a phone, a side panel from `sm`
            // up: 20rem beside a 360px screen leaves nothing to look at.
            className="absolute inset-0 z-10 flex flex-col gap-4 overflow-y-auto border-border bg-surface p-4 sm:static sm:z-auto sm:w-80 sm:shrink-0 sm:border-l"
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

            {/* On a phone the panel covers the console, so it needs its own way
                back. On a wider screen the toolbar button is still visible. */}
            <ToolbarButton onClick={() => setClipboardOpen(false)}>
              <span className="sm:hidden">Back to the console</span>
              <span className="hidden sm:inline">Close panel</span>
            </ToolbarButton>
          </aside>
        )}
      </div>

      {/*
        The field that summons the on-screen keyboard. It has to be focusable,
        so it cannot be `hidden` or `display: none`; it is instead made
        invisible and parked out of the way. autoCapitalize and the rest are
        off because a phone keyboard's helpfulness — capitalising the first
        letter of a command, correcting a hostname — is wrong in a terminal.
      */}
      <textarea
        ref={keyboardRef}
        aria-hidden="true"
        tabIndex={-1}
        autoCapitalize="off"
        autoCorrect="off"
        autoComplete="off"
        spellCheck={false}
        onInput={onKeyboardInput}
        onKeyDown={onKeyboardKeyDown}
        onBlur={() => setKeyboardOn(false)}
        className="pointer-events-none absolute left-0 top-0 h-px w-px resize-none border-0 bg-transparent p-0 text-transparent opacity-0 outline-none"
      />

      {keyboardOn && (
        <div
          aria-label="Keys"
          role="group"
          className="flex items-center gap-2 overflow-x-auto border-t border-border bg-surface px-3 py-2"
        >
          {/* The keys a phone keyboard has no room for, and which a console is
              close to unusable without. */}
          <KeyButton onPress={() => pressKey(XK_Escape, 'Escape')}>Esc</KeyButton>
          <KeyButton onPress={() => pressKey(XK_Tab, 'Tab')}>Tab</KeyButton>
          <KeyButton onPress={() => pressKey(XK_Return, 'Enter')}>Enter</KeyButton>
          <KeyButton onPress={() => pressKey(XK_BackSpace, 'Backspace')} label="Backspace">
            ⌫
          </KeyButton>
          <KeyButton onPress={() => pressKey(XK_Left, 'ArrowLeft')} label="Left">
            ←
          </KeyButton>
          <KeyButton onPress={() => pressKey(XK_Down, 'ArrowDown')} label="Down">
            ↓
          </KeyButton>
          <KeyButton onPress={() => pressKey(XK_Up, 'ArrowUp')} label="Up">
            ↑
          </KeyButton>
          <KeyButton onPress={() => pressKey(XK_Right, 'ArrowRight')} label="Right">
            →
          </KeyButton>
        </div>
      )}
    </div>
  )
}

/**
 * One key on the strip above the soft keyboard.
 *
 * It acts on pointerdown with the default prevented, so the press never moves
 * focus off the hidden field — a blur would dismiss the on-screen keyboard,
 * and pressing Tab should not close the keyboard you are typing on.
 */
function KeyButton({
  children,
  onPress,
  label,
}: {
  children: React.ReactNode
  onPress: () => void
  label?: string
}) {
  return (
    <button
      aria-label={label}
      onPointerDown={(event) => {
        event.preventDefault()
        onPress()
      }}
      className="min-w-[2.75rem] rounded-md border border-border px-3 py-2 text-sm hover:bg-surface-raised"
    >
      {children}
    </button>
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
