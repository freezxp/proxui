import { forwardRef, useEffect, useImperativeHandle, useRef } from 'react'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import { WebLinksAddon } from '@xterm/addon-web-links'
import '@xterm/xterm/css/xterm.css'

/**
 * The terminal itself: xterm.js bound to the portal's SSH WebSocket.
 *
 * The wire protocol is deliberately two-shaped. Binary frames are terminal
 * bytes in both directions — a keystroke is a byte, a control sequence is
 * bytes, and wrapping either in JSON would cost an allocation per character
 * and mangle anything that is not valid UTF-8. Text frames carry control
 * messages, of which there is exactly one: the window changed size. Because
 * the two frame types cannot collide, neither needs a header.
 */

/** The keys a phone's on-screen keyboard does not have, and a shell needs. */
export type SpecialKey =
  'escape' | 'tab' | 'up' | 'down' | 'left' | 'right' | 'home' | 'end' | 'pageup' | 'pagedown'

/** How much of the buffer to lift out as text: what is on screen, or
 *  everything the scrollback still holds. */
export type SnapshotScope = 'screen' | 'all'

/** Sticky modifiers: armed by a tap, spent on the next keystroke. */
export interface Modifiers {
  ctrl: boolean
  alt: boolean
}

export interface TerminalHandle {
  /** Types text into the session, as if it had been typed. */
  paste(text: string): void
  /** Sends a key the soft keyboard cannot, with any armed modifier applied. */
  press(key: SpecialKey): void
  /** Arms or disarms a modifier for the next keystroke, from either the bar
   *  or the on-screen keyboard. */
  toggleModifier(mod: keyof Modifiers): void
  /** Sends a control character directly — Ctrl+C and friends, which are the
   *  combinations people reach for often enough that arming a modifier first
   *  is a tap too many. */
  control(letter: string): void
  /** The current selection, or '' when nothing is selected. */
  selection(): string
  clearSelection(): void
  /** The screen, or the whole scrollback, as plain text. This is the way out
   *  of a full-screen program that has taken the mouse: the bytes are already
   *  in the browser's buffer, so lifting them does not need the guest's
   *  cooperation and does not need a drag the guest is intercepting. */
  snapshot(scope: SnapshotScope): string
  /** Whether the guest has asked for mouse events — tmux, vim and htop all do
   *  — which is what stops a drag from selecting anything. */
  mouseReporting(): boolean
  clear(): void
  focus(): void
  fit(): void
}

interface Props {
  wsUrl: string
  fontSize: number
  /** Called once the socket is open, and again when it closes, with a reason
   *  the operator can act on. */
  onOpen: () => void
  onClose: (detail: string, code: number) => void
  /** A right-click with no selection asks to paste; the page decides whether
   *  the clipboard can be read or the paste panel has to open. */
  onPasteRequest: () => void
  /** A right-click with a selection copies it. */
  onCopyRequest: (text: string) => void
  /** Reports the armed modifiers so the key bar can show which are live.
   *  They are held here rather than in the page because spending one is
   *  something only the input path can observe. */
  onModifiersChange?: (mods: Modifiers) => void
}

/** Exported for tests: the encodings below are the whole correctness surface
 *  of the key bar, and they are easier to pin directly than through a canvas
 *  xterm cannot render in jsdom. */

/** toControl turns a typed character into the control code Ctrl+that would
 *  produce. Ctrl+C is \x03 because C is 67 and the control range starts 64
 *  below — the same arithmetic a real terminal does. */
export function toControl(data: string): string {
  if (data.length !== 1) return data
  if (data === ' ') return '\x00'
  if (data === '?') return '\x7f'
  const code = data.toUpperCase().charCodeAt(0)
  // @ A-Z [ \ ] ^ _ are the characters with a control equivalent.
  if (code >= 64 && code <= 95) return String.fromCharCode(code - 64)
  return data
}

/** keySequence renders a key as the bytes the far end expects.
 *
 *  Two encodings are in play and both matter. Cursor keys change form when the
 *  guest turns on application cursor mode — vim and less do — so the mode is
 *  read from the terminal rather than assumed. And a modifier is not a prefix
 *  but a parameter: Ctrl+Left is `ESC [ 1 ; 5 D`, which is what makes word
 *  jumping work in a shell. */
export function keySequence(key: SpecialKey, mods: Modifiers, appCursor: boolean): string {
  // 1 + shift(1) + alt(2) + ctrl(4), per the xterm modifier encoding.
  const m = 1 + (mods.alt ? 2 : 0) + (mods.ctrl ? 4 : 0)
  const cursor = (final: string) => {
    if (m > 1) return `\x1b[1;${m}${final}`
    return appCursor ? `\x1bO${final}` : `\x1b[${final}`
  }
  const tilde = (n: number) => (m > 1 ? `\x1b[${n};${m}~` : `\x1b[${n}~`)

  switch (key) {
    case 'up':
      return cursor('A')
    case 'down':
      return cursor('B')
    case 'right':
      return cursor('C')
    case 'left':
      return cursor('D')
    case 'home':
      return cursor('H')
    case 'end':
      return cursor('F')
    case 'pageup':
      return tilde(5)
    case 'pagedown':
      return tilde(6)
    case 'escape':
      return '\x1b'
    case 'tab':
      return '\t'
  }
}

/** The parts of xterm's buffer bufferText reads, named here so a test can
 *  supply them: xterm fills a real buffer only against a canvas. */
export interface BufferView {
  readonly length: number
  readonly viewportY: number
  getLine(
    y: number,
  ): { isWrapped: boolean; translateToString(trimRight?: boolean): string } | undefined
}

/** bufferText renders the buffer as the text a person meant to copy.
 *
 *  Two things separate this from joining every row with a newline. A row whose
 *  successor is a continuation of it is not a line: breaking there would cut a
 *  command in half at the width of the window rather than where it ends, and
 *  trimming its right edge would eat the space between two words. And the
 *  blank rows below the last output are the empty part of the screen, not
 *  content — pasting them somewhere else is pasting nothing. */
export function bufferText(buffer: BufferView, rows: number, scope: SnapshotScope): string {
  const start = scope === 'screen' ? Math.max(0, buffer.viewportY) : 0
  const end = scope === 'screen' ? Math.min(buffer.length, buffer.viewportY + rows) : buffer.length

  const lines: string[] = []
  let line = ''
  for (let y = start; y < end; y++) {
    const row = buffer.getLine(y)
    if (!row) continue
    const wraps = y + 1 < end && buffer.getLine(y + 1)?.isWrapped === true
    line += row.translateToString(!wraps)
    if (!wraps) {
      lines.push(line)
      line = ''
    }
  }
  if (line !== '') lines.push(line)
  while (lines.length > 0 && lines[lines.length - 1] === '') lines.pop()
  return lines.join('\n')
}

// Close codes the bridge sends (docs/29-ssh-terminal.md). Anything else is an
// unexpected drop rather than a decision.
const CLOSE_REASONS: Record<number, string> = {
  4000: 'The session closed after 30 minutes without activity.',
  4001: 'The session reached its maximum length of 8 hours.',
  4002: 'An administrator ended this session.',
  4003: 'The connection to the guest was lost.',
  4005: 'The session is no longer open. Connect again to start a new one.',
}

export const SshTerminal = forwardRef<TerminalHandle, Props>(function SshTerminal(
  { wsUrl, fontSize, onOpen, onClose, onPasteRequest, onCopyRequest, onModifiersChange },
  ref,
) {
  const holder = useRef<HTMLDivElement>(null)
  const term = useRef<Terminal | null>(null)
  const fit = useRef<FitAddon | null>(null)
  const socket = useRef<WebSocket | null>(null)

  // The callbacks are held in a ref so that changing one does not tear the
  // socket down and reconnect — which would need a new ticket, and the
  // password to be typed again.
  const handlers = useRef({ onOpen, onClose, onPasteRequest, onCopyRequest, onModifiersChange })
  handlers.current = { onOpen, onClose, onPasteRequest, onCopyRequest, onModifiersChange }

  // Armed modifiers live in a ref, not state: they are read on the input path,
  // which must not depend on a render having happened first.
  const mods = useRef<Modifiers>({ ctrl: false, alt: false })
  const setMods = (next: Modifiers) => {
    mods.current = next
    handlers.current.onModifiersChange?.({ ...next })
  }
  const spend = () => {
    if (mods.current.ctrl || mods.current.alt) setMods({ ctrl: false, alt: false })
  }
  const sendRaw = (data: string) => {
    const ws = socket.current
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(new TextEncoder().encode(data))
  }

  useImperativeHandle(ref, () => ({
    paste: (text: string) => term.current?.paste(text),
    press: (key: SpecialKey) => {
      const terminal = term.current
      if (!terminal) return
      // The guest decides how cursor keys are encoded; asking it beats
      // guessing, which is the difference between arrows that work in vim and
      // arrows that print letters.
      const appCursor = terminal.modes?.applicationCursorKeysMode === true
      sendRaw(keySequence(key, mods.current, appCursor))
      spend()
      terminal.focus()
    },
    control: (letter: string) => {
      const code = letter.toUpperCase().charCodeAt(0)
      if (code < 64 || code > 95) return
      sendRaw(String.fromCharCode(code - 64))
      spend()
      term.current?.focus()
    },
    toggleModifier: (mod: keyof Modifiers) => {
      setMods({ ...mods.current, [mod]: !mods.current[mod] })
      term.current?.focus()
    },
    selection: () => term.current?.getSelection() ?? '',
    clearSelection: () => term.current?.clearSelection(),
    snapshot: (scope: SnapshotScope) => {
      const terminal = term.current
      if (!terminal) return ''
      return bufferText(terminal.buffer.active, terminal.rows, scope)
    },
    mouseReporting: () => {
      const mode = term.current?.modes?.mouseTrackingMode
      return mode !== undefined && mode !== 'none'
    },
    clear: () => term.current?.clear(),
    focus: () => term.current?.focus(),
    fit: () => fit.current?.fit(),
  }))

  useEffect(() => {
    if (!holder.current) return

    const terminal = new Terminal({
      fontSize,
      fontFamily:
        'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, "Liberation Mono", monospace',
      cursorBlink: true,
      // Deep enough to scroll back through a build log, bounded so a runaway
      // `yes` does not grow the tab without limit.
      scrollback: 10000,
      // Matches the portal's dark surfaces rather than xterm's default black,
      // which reads as a hole punched in the page.
      theme: {
        background: '#0b0f17',
        foreground: '#d6deeb',
        cursor: '#d6deeb',
        selectionBackground: '#2d3f5e',
      },
      allowProposedApi: true,
    })
    const fitAddon = new FitAddon()
    terminal.loadAddon(fitAddon)
    terminal.loadAddon(new WebLinksAddon())
    terminal.open(holder.current)
    fitAddon.fit()

    term.current = terminal
    fit.current = fitAddon

    const url = new URL(wsUrl, window.location.origin)
    url.protocol = url.protocol.replace('http', 'ws')
    url.searchParams.set('cols', String(terminal.cols))
    url.searchParams.set('rows', String(terminal.rows))
    const ws = new WebSocket(url.toString())
    ws.binaryType = 'arraybuffer'
    socket.current = ws

    const encoder = new TextEncoder()

    ws.onopen = () => {
      handlers.current.onOpen()
      terminal.focus()
    }
    ws.onmessage = (event) => {
      // Binary is terminal output. A text frame would be a control message,
      // and the server currently sends none — ignoring them keeps the browser
      // forward-compatible with one that does.
      if (event.data instanceof ArrayBuffer) terminal.write(new Uint8Array(event.data))
    }
    ws.onclose = (event) => {
      const detail =
        CLOSE_REASONS[event.code] ??
        (event.wasClean ? 'The session ended.' : 'The connection dropped.')
      handlers.current.onClose(detail, event.code)
    }

    // Keystrokes out. onData covers typing, pasting and anything else that
    // produces input, which is why paste() above simply feeds it.
    const typed = terminal.onData((data) => {
      if (ws.readyState !== WebSocket.OPEN) return
      // An armed modifier applies to whatever is typed next, wherever it came
      // from: tapping Ctrl and then C on the phone's keyboard has to interrupt
      // the same way a physical Ctrl+C does.
      let out = data
      if (mods.current.ctrl) out = toControl(out)
      if (mods.current.alt) out = '\x1b' + out
      spend()
      ws.send(encoder.encode(out))
    })
    // Sequences the terminal answers on its own behalf — a cursor position
    // report, say — travel the same way.
    const replied = terminal.onBinary((data) => {
      if (ws.readyState !== WebSocket.OPEN) return
      const bytes = new Uint8Array(data.length)
      for (let i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) & 0xff
      ws.send(bytes)
    })

    const sendSize = () => {
      if (ws.readyState !== WebSocket.OPEN) return
      ws.send(JSON.stringify({ type: 'resize', cols: terminal.cols, rows: terminal.rows }))
    }
    const resized = terminal.onResize(sendSize)

    // ResizeObserver rather than a window listener: the terminal also changes
    // width when the file panel opens beside it, which resizes no window.
    const observer = new ResizeObserver(() => {
      try {
        fitAddon.fit()
      } catch {
        /* the element is momentarily detached during a route change */
      }
    })
    observer.observe(holder.current)

    // Ctrl+Shift+C / Ctrl+Shift+V are the terminal convention, and have to be
    // intercepted before xterm treats them as input. Returning false tells
    // xterm the key was handled here.
    terminal.attachCustomKeyEventHandler((event) => {
      if (event.type !== 'keydown' || !event.ctrlKey || !event.shiftKey) return true
      const key = event.key.toLowerCase()
      if (key === 'c') {
        const selection = terminal.getSelection()
        if (selection !== '') {
          handlers.current.onCopyRequest(selection)
          return false
        }
        // With nothing selected, Ctrl+Shift+C should still interrupt rather
        // than be swallowed — but that is Ctrl+C's job, so let it through.
        return true
      }
      if (key === 'v') {
        handlers.current.onPasteRequest()
        return false
      }
      return true
    })

    return () => {
      observer.disconnect()
      typed.dispose()
      replied.dispose()
      resized.dispose()
      // 1000 is a clean close; the server treats a socket that simply goes
      // away as the operator leaving, and keeps the SSH session alive until
      // the idle sweep, so a reload does not cost another password.
      if (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING) ws.close(1000)
      terminal.dispose()
      term.current = null
      fit.current = null
      socket.current = null
    }
    // wsUrl alone: a new ticket means a new session, and nothing else here
    // justifies tearing down a live terminal.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [wsUrl])

  // Font size changes in place. Refitting afterwards is what keeps the guest's
  // idea of the window matching what is on screen.
  useEffect(() => {
    if (!term.current) return
    term.current.options.fontSize = fontSize
    try {
      fit.current?.fit()
    } catch {
      /* not yet laid out */
    }
  }, [fontSize])

  return (
    <div
      ref={holder}
      className="h-full w-full"
      // Right-click is the other half of terminal copy/paste: with a selection
      // it copies, without one it pastes. The browser menu would offer neither
      // usefully over a canvas.
      onContextMenu={(event) => {
        event.preventDefault()
        const selection = term.current?.getSelection() ?? ''
        if (selection !== '') handlers.current.onCopyRequest(selection)
        else handlers.current.onPasteRequest()
      }}
    />
  )
})
