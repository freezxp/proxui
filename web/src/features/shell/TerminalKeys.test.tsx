import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { keySequence, toControl, type TerminalHandle } from './SshTerminal'
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

describe('the key bar', () => {
  function renderBar(modifiers = { ctrl: false, alt: false }) {
    const handle: TerminalHandle = {
      paste: vi.fn(),
      press: vi.fn(),
      control: vi.fn(),
      toggleModifier: vi.fn(),
      selection: vi.fn(() => ''),
      clearSelection: vi.fn(),
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
