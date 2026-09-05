import { useCallback, useEffect, useRef } from 'react'
import type { SnapshotScope } from './SshTerminal'

/**
 * Copying out of a program that has taken the mouse (SSH-08).
 *
 * tmux, vim, htop and anything else that turns on mouse reporting ask the
 * terminal to send them every click and drag. xterm.js obliges, which is what
 * makes those programs usable — and which means a drag no longer selects
 * anything to copy. A desktop operator can hold Shift to take the mouse back
 * for one drag, the same escape hatch xterm itself has. On a phone there is no
 * Shift, and dragging on the canvas scrolls rather than selects.
 *
 * So the way out is not to fight for the drag but to leave the canvas: this
 * panel shows the buffer as ordinary text, which selects the way text does
 * everywhere — a drag with a mouse, a long press with a finger — and offers
 * copying the whole thing for the case where selecting precisely is more
 * trouble than it is worth. The bytes are already in the browser, so none of
 * this asks the guest for anything or interrupts what is running.
 *
 * It is a snapshot, deliberately: text that reflowed under a live selection
 * would lose the selection every time the screen redrew, and a screen with
 * tmux on it redraws constantly.
 */

export function TerminalSelect({
  text,
  scope,
  fontSize,
  mouseGrabbed,
  onScope,
  onCopy,
  onNotice,
  onClose,
}: {
  /** The buffer as text, taken when the panel opened or the scope changed. */
  text: string
  scope: SnapshotScope
  /** Matched to the terminal's own size, so columns line up with what was
   *  on screen a moment ago and a wide table still reads as a table. */
  fontSize: number
  /** Whether a full-screen program is holding the mouse, which is the reason
   *  most people arrive here and worth saying out loud. */
  mouseGrabbed: boolean
  onScope: (scope: SnapshotScope) => void
  onCopy: (text: string) => void
  onNotice: (message: string) => void
  onClose: () => void
}) {
  const body = useRef<HTMLPreElement>(null)
  // The last selection that was not empty. Tapping a button can collapse the
  // selection before the click arrives — preventing the default on mousedown
  // stops that for a mouse, but a touch selection is dismissed by the tap
  // itself in some browsers, and then the button would copy nothing.
  const held = useRef('')

  useEffect(() => {
    const remember = () => {
      const selection = document.getSelection()
      if (!selection || selection.isCollapsed) return
      const anchor = selection.anchorNode
      if (body.current && anchor && body.current.contains(anchor))
        held.current = selection.toString()
    }
    document.addEventListener('selectionchange', remember)
    return () => document.removeEventListener('selectionchange', remember)
  }, [])

  // Focus moves off the terminal for as long as the panel is up: it is covered,
  // and typing into a guest you cannot see is how a command lands in the wrong
  // window. It also puts Escape and the scroll keys where the reader expects.
  useEffect(() => {
    body.current?.focus({ preventScroll: true })
  }, [])

  // A fresh snapshot is a different body of text; whatever was selected in the
  // old one no longer means anything.
  useEffect(() => {
    held.current = ''
  }, [text])

  const copySelected = useCallback(() => {
    const selection = document.getSelection()
    const live =
      selection && !selection.isCollapsed && body.current?.contains(selection.anchorNode ?? null)
        ? selection.toString()
        : ''
    const chosen = live !== '' ? live : held.current
    if (chosen === '') {
      onNotice('Select some text first, or copy everything.')
      return
    }
    onCopy(chosen)
  }, [onCopy, onNotice])

  return (
    <div
      className="absolute inset-0 z-20 flex flex-col bg-[#0b0f17]"
      role="dialog"
      aria-label="Select text to copy"
      onKeyDown={(event) => {
        if (event.key === 'Escape') onClose()
      }}
    >
      <div className="flex flex-wrap items-center gap-1 border-b border-border bg-surface px-2 py-1.5">
        <p className="mr-auto min-w-0 py-0.5 pr-2 text-xs text-muted">
          {mouseGrabbed
            ? 'A full-screen program has the mouse. Select here instead.'
            : 'Select the text you want, then copy it.'}
        </p>
        <PanelButton pressed={scope === 'screen'} onClick={() => onScope('screen')}>
          Screen
        </PanelButton>
        <PanelButton pressed={scope === 'all'} onClick={() => onScope('all')}>
          With scrollback
        </PanelButton>
        <span className="mx-1 h-5 w-px shrink-0 bg-border" aria-hidden="true" />
        <PanelButton onClick={copySelected} keepSelection>
          Copy selection
        </PanelButton>
        <PanelButton onClick={() => onCopy(text)} keepSelection>
          Copy all
        </PanelButton>
        <PanelButton onClick={onClose}>Done</PanelButton>
      </div>

      <pre
        ref={body}
        tabIndex={-1}
        aria-label="Terminal text"
        // whitespace-pre and no wrapping: a wrapped line would put the columns
        // of a table or a tmux split somewhere they were not, and the point of
        // this panel is to hand back what was on screen.
        className="min-h-0 flex-1 select-text overflow-auto whitespace-pre px-3 py-2 font-mono leading-snug text-content"
        style={{ fontSize }}
      >
        {text === '' ? 'The terminal is empty.' : text}
      </pre>
    </div>
  )
}

function PanelButton({
  children,
  onClick,
  pressed,
  keepSelection,
}: {
  children: React.ReactNode
  onClick: () => void
  pressed?: boolean
  /** Buttons that act on the selection must not take it away by being clicked,
   *  which is what focusing them would do. */
  keepSelection?: boolean
}) {
  return (
    <button
      type="button"
      onMouseDown={keepSelection ? (event) => event.preventDefault() : undefined}
      onClick={onClick}
      aria-pressed={pressed}
      className={`shrink-0 rounded-md border px-2 py-1 text-xs ${
        pressed ? 'border-accent bg-accent/10' : 'border-border hover:bg-surface-inset'
      }`}
    >
      {children}
    </button>
  )
}
