import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { bufferText, keySequence, toControl, type TerminalHandle } from './SshTerminal'
import { TerminalKeys } from './TerminalKeys'

/**
 * The key bar's correctness is almost entirely in the bytes it sends: a wrong
 * escape sequence does not throw, it prints "^[[A" into somebody's shell.
 */

describe('key encoding', () => {
  it('sends the cursor sequences a shell expects', () => {
    const plain = { ctrl: false, alt: false }
    expect(keySequence('up', plain, false)).toBe('\x1b[A')
    expect(keySequence('down', plain, false)).toBe('\x1b[B')
    expect(keySequence('right', plain, false)).toBe('\x1b[C')
    expect(keySequence('left', plain, false)).toBe('\x1b[D')
  })

  it('switches form when the guest turns on application cursor mode', () => {
    // vim and less do this. Sending the wrong one prints letters instead of
    // moving the cursor, which is the classic broken-web-terminal symptom.
    const plain = { ctrl: false, alt: false }
    expect(keySequence('up', plain, true)).toBe('\x1bOA')
    expect(keySequence('home', plain, true)).toBe('\x1bOH')
  })

  it('encodes modifiers as a parameter, not a prefix', () => {
    // Ctrl+Left is what makes word-jumping work; ESC ESC [ D would not.
    expect(keySequence('left', { ctrl: true, alt: false }, false)).toBe('\x1b[1;5D')
    expect(keySequence('right', { ctrl: false, alt: true }, false)).toBe('\x1b[1;3C')
    expect(keySequence('up', { ctrl: true, alt: true }, false)).toBe('\x1b[1;7A')
    // A modifier wins over application cursor mode, as xterm specifies.
    expect(keySequence('up', { ctrl: true, alt: false }, true)).toBe('\x1b[1;5A')
  })

  it('sends page keys in tilde form', () => {
    const plain = { ctrl: false, alt: false }
    expect(keySequence('pageup', plain, false)).toBe('\x1b[5~')
    expect(keySequence('pagedown', plain, false)).toBe('\x1b[6~')
    expect(keySequence('pageup', { ctrl: true, alt: false }, false)).toBe('\x1b[5;5~')
  })

  it('sends escape and tab as themselves', () => {
    const plain = { ctrl: false, alt: false }
    expect(keySequence('escape', plain, false)).toBe('\x1b')
    expect(keySequence('tab', plain, false)).toBe('\t')
  })

  it('turns a typed character into its control code', () => {
    expect(toControl('c')).toBe('\x03')
    expect(toControl('C')).toBe('\x03')
    expect(toControl('d')).toBe('\x04')
    expect(toControl(' ')).toBe('\x00')
    // Anything with no control equivalent is left alone rather than mangled.
    expect(toControl('7')).toBe('7')
    expect(toControl('éé')).toBe('éé')
  })
})

/** A buffer of fixed-width rows, the shape xterm exposes and jsdom cannot
 *  produce: xterm only fills a real buffer against a canvas it can measure. */
function fakeBuffer(rows: string[], viewportY = 0, wrapped: number[] = []) {
  const width = Math.max(0, ...rows.map((row) => row.length))
  return {
    length: rows.length,
    viewportY,
    getLine(y: number) {
      const row = rows[y]
      if (row === undefined) return undefined
      return {
        isWrapped: wrapped.includes(y),
        translateToString: (trimRight?: boolean) =>
          trimRight === false ? row.padEnd(width, ' ') : row.replace(/\s+$/, ''),
      }
    },
  }
}

describe('lifting the buffer out as text', () => {
  it('takes what is on screen, not the scrollback above it', () => {
    // The whole point of the panel: copy what tmux is showing right now.
    const buffer = fakeBuffer(['scrolled off', 'first', 'second'], 1)
    expect(bufferText(buffer, 2, 'screen')).toBe('first\nsecond')
    expect(bufferText(buffer, 2, 'all')).toBe('scrolled off\nfirst\nsecond')
  })

  it('joins a line the window broke in half', () => {
    // A command longer than the window is one line to whoever typed it, and
    // pasting it back has to run rather than break at column 80.
    const buffer = fakeBuffer(['journalctl -u ', 'nginx --since'], 0, [1])
    expect(bufferText(buffer, 2, 'all')).toBe('journalctl -u nginx --since')
  })

  it('drops the empty part of the screen', () => {
    // A screen is mostly blank rows below the prompt; copying them would paste
    // a page of nothing.
    expect(bufferText(fakeBuffer(['$ uptime', '', '', '']), 4, 'screen')).toBe('$ uptime')
  })

  it('keeps blank rows that have output under them', () => {
    // Blank lines inside output are output — a paragraph break in a log.
    expect(bufferText(fakeBuffer(['one', '', 'two']), 3, 'screen')).toBe('one\n\ntwo')
  })
})

describe('the key bar', () => {
  function renderBar(modifiers = { ctrl: false, alt: false }) {
    const handle: TerminalHandle = {
      paste: vi.fn(),
      press: vi.fn(),
      control: vi.fn(),
      toggleModifier: vi.fn(),
      selection: vi.fn(() => ''),
      clearSelection: vi.fn(),
      snapshot: vi.fn(() => ''),
      mouseReporting: vi.fn(() => false),
      clear: vi.fn(),
      focus: vi.fn(),
      fit: vi.fn(),
    }
    render(<TerminalKeys terminal={{ current: handle }} modifiers={modifiers} />)
    return handle
  }

  it('sends the key that was tapped', async () => {
    const handle = renderBar()
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: /Up — previous command/ }))
    expect(handle.press).toHaveBeenCalledWith('up')

    await user.click(screen.getByRole('button', { name: /Tab/ }))
    expect(handle.press).toHaveBeenCalledWith('tab')
  })

  it('offers the combinations people actually use, in one tap', async () => {
    const handle = renderBar()
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: /Interrupt/ }))
    expect(handle.control).toHaveBeenCalledWith('c')

    await user.click(screen.getByRole('button', { name: /Search command history/ }))
    expect(handle.control).toHaveBeenCalledWith('r')
  })

  it('arms a modifier rather than sending it', async () => {
    const handle = renderBar()
    const user = userEvent.setup()

    await user.click(screen.getByRole('button', { name: /^Ctrl/ }))
    expect(handle.toggleModifier).toHaveBeenCalledWith('ctrl')
    expect(handle.press).not.toHaveBeenCalled()
  })

  it('shows an armed modifier as pressed', () => {
    renderBar({ ctrl: true, alt: false })
    // Without this the operator cannot tell whether Ctrl is live, and the
    // sticky-modifier model becomes guesswork.
    expect(screen.getByRole('button', { name: /^Ctrl/ }).getAttribute('aria-pressed')).toBe('true')
    expect(screen.getByRole('button', { name: /^Alt/ }).getAttribute('aria-pressed')).toBe('false')
  })
})
