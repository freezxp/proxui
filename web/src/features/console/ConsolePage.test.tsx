import { beforeEach, describe, expect, it, vi } from 'vitest'
import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'

/**
 * A stand-in for noVNC's RFB, recording what the page asks it to do.
 *
 * Declared through vi.hoisted because vi.mock is lifted to the top of the
 * module, above any ordinary declaration it would otherwise close over.
 */
const { FakeRFB } = vi.hoisted(() => {
  class FakeRFB {
    static last: FakeRFB | null = null
    private listeners = new Map<string, EventListener[]>()
    pasted: string[] = []
    scaleViewport = false
    clipViewport = false
    background = ''

    constructor() {
      FakeRFB.last = this
    }

    addEventListener(type: string, fn: EventListener) {
      this.listeners.set(type, [...(this.listeners.get(type) ?? []), fn])
    }

    emit(type: string, detail?: unknown) {
      for (const fn of this.listeners.get(type) ?? []) {
        fn(new CustomEvent(type, { detail }) as Event)
      }
    }

    clipboardPasteFrom(text: string) {
      this.pasted.push(text)
    }

    keys: Array<{ keysym: number; code: string | null }> = []
    sendKey(keysym: number, code: string | null) {
      this.keys.push({ keysym, code })
    }

    sendCtrlAltDel() {}
    disconnect() {}
  }
  return { FakeRFB }
})

type FakeRFBInstance = InstanceType<typeof FakeRFB>

vi.mock('@novnc/novnc', () => ({ default: FakeRFB }))

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: {
      get: vi.fn().mockResolvedValue({ id: 'vm1', name: 'web-01', state: 'running' }),
      post: vi.fn().mockResolvedValue({
        session_id: 's1',
        ws_url: '/ws/console/ticket-1',
        expires_in: 30,
      }),
    },
  }
})

// jsdom would try to open a real socket; the page only needs the object to
// exist and to accept a close listener.
class FakeSocket {
  binaryType = ''
  addEventListener() {}
  close() {}
}
vi.stubGlobal('WebSocket', FakeSocket)

import { ConsolePage } from './ConsolePage'

async function renderConnectedConsole() {
  render(
    <MemoryRouter initialEntries={['/vms/vm1/console']}>
      <Routes>
        <Route path="/vms/:vmId/console" element={<ConsolePage />} />
      </Routes>
    </MemoryRouter>,
  )
  await waitFor(() => expect(FakeRFB.last).not.toBeNull())
  const rfb = FakeRFB.last as FakeRFBInstance
  rfb.emit('connect')
  await screen.findByRole('button', { name: 'Clipboard' })
  return rfb
}

describe('console clipboard', () => {
  beforeEach(() => {
    FakeRFB.last = null
  })

  it('stays out of the way until asked for', async () => {
    await renderConnectedConsole()
    expect(screen.queryByRole('complementary', { name: 'Clipboard' })).toBeNull()
  })

  it('sends typed text to the VM', async () => {
    const person = userEvent.setup()
    const rfb = await renderConnectedConsole()

    await person.click(screen.getByRole('button', { name: 'Clipboard' }))
    await person.type(screen.getByLabelText('Send to the VM'), 'apt update')
    await person.click(screen.getByRole('button', { name: 'Send' }))

    expect(rfb.pasted).toEqual(['apt update'])
  })

  it('will not send nothing', async () => {
    const person = userEvent.setup()
    const rfb = await renderConnectedConsole()

    await person.click(screen.getByRole('button', { name: 'Clipboard' }))
    expect(screen.getByRole('button', { name: 'Send' })).toHaveProperty('disabled', true)
    expect(rfb.pasted).toEqual([])
  })

  it('shows what was copied inside the VM, even if the panel was shut', async () => {
    const person = userEvent.setup()
    const rfb = await renderConnectedConsole()

    // The guest pushes cut text unasked, so this arrives before the panel is
    // opened. Dropping it would make the feature look intermittent.
    rfb.emit('clipboard', { text: 'ssh-ed25519 AAAAC3Nz' })
    await person.click(screen.getByRole('button', { name: 'Clipboard' }))

    expect(screen.getByLabelText('Copied in the VM')).toHaveProperty(
      'value',
      'ssh-ed25519 AAAAC3Nz',
    )
  })

  it('copies the guest text to this computer', async () => {
    const person = userEvent.setup()
    const writeText = vi.fn().mockResolvedValue(undefined)
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })

    const rfb = await renderConnectedConsole()
    rfb.emit('clipboard', { text: 'from-the-guest' })
    await person.click(screen.getByRole('button', { name: 'Clipboard' }))
    await person.click(screen.getByRole('button', { name: 'Copy' }))

    expect(writeText).toHaveBeenCalledWith('from-the-guest')
    vi.unstubAllGlobals()
    vi.stubGlobal('WebSocket', FakeSocket)
  })

  it('falls back to selecting the text when the clipboard is refused', async () => {
    const person = userEvent.setup()
    const writeText = vi.fn().mockRejectedValue(new Error('denied'))
    vi.stubGlobal('navigator', { ...navigator, clipboard: { writeText } })

    const rfb = await renderConnectedConsole()
    rfb.emit('clipboard', { text: 'from-the-guest' })
    await person.click(screen.getByRole('button', { name: 'Clipboard' }))
    await person.click(screen.getByRole('button', { name: 'Copy' }))

    // No permission and no secure context is the plain-HTTP LAN case, which
    // must leave the user one Ctrl+C away rather than at a dead end.
    await screen.findByText('Press Ctrl+C to copy the selected text.')

    vi.unstubAllGlobals()
    vi.stubGlobal('WebSocket', FakeSocket)
  })
})

describe('console soft keyboard', () => {
  beforeEach(() => {
    FakeRFB.last = null
  })

  // A touch device has no physical keys to capture, so the hidden field is
  // what the on-screen keyboard types into. It must be focusable, which rules
  // out `hidden` and `display: none`.
  function keyboardField(): HTMLTextAreaElement {
    const field = document.querySelector<HTMLTextAreaElement>('textarea[aria-hidden="true"]')
    if (!field) throw new Error('no hidden keyboard field')
    return field
  }

  /** What a keyboard does: set the value, put the caret after it, fire input. */
  function typeInto(field: HTMLTextAreaElement, value: string) {
    field.value = value
    field.setSelectionRange(value.length, value.length)
    fireEvent.input(field, { target: { value } })
  }

  it('focuses the hidden field so the on-screen keyboard opens', async () => {
    const person = userEvent.setup()
    await renderConnectedConsole()

    await person.click(screen.getByRole('button', { name: 'Keyboard' }))
    expect(document.activeElement).toBe(keyboardField())
  })

  it('turns typed characters into key events', async () => {
    const person = userEvent.setup()
    const rfb = await renderConnectedConsole()
    await person.click(screen.getByRole('button', { name: 'Keyboard' }))

    const field = keyboardField()
    typeInto(field, field.value + 'ls')

    // Latin-1 is a one-to-one mapping, so these are the codepoints.
    expect(rfb.keys.map((k) => k.keysym)).toEqual(['l'.charCodeAt(0), 's'.charCodeAt(0)])
  })

  // Swipe typing and autocorrect replace whole words rather than sending
  // keystrokes, and the guest has to receive the deletions that implies.
  it('reproduces a word the keyboard replaced as backspaces and characters', async () => {
    const person = userEvent.setup()
    const rfb = await renderConnectedConsole()
    await person.click(screen.getByRole('button', { name: 'Keyboard' }))

    const field = keyboardField()
    const start = field.value
    typeInto(field, start + 'cd')
    rfb.keys.length = 0
    typeInto(field, start + 'ls')

    expect(rfb.keys).toEqual([
      { keysym: 0xff08, code: 'Backspace' },
      { keysym: 0xff08, code: 'Backspace' },
      { keysym: 'l'.charCodeAt(0), code: null },
      { keysym: 's'.charCodeAt(0), code: null },
    ])
  })

  it('sends Enter rather than letting it insert a newline', async () => {
    const person = userEvent.setup()
    const rfb = await renderConnectedConsole()
    await person.click(screen.getByRole('button', { name: 'Keyboard' }))

    const event = fireEvent.keyDown(keyboardField(), { key: 'Enter' })

    expect(rfb.keys).toEqual([{ keysym: 0xff0d, code: 'Enter' }])
    // Not preventing the default would put a newline in the field and send
    // nothing to the guest.
    expect(event).toBe(false)
  })

  it('offers the keys a phone keyboard does not have', async () => {
    const person = userEvent.setup()
    const rfb = await renderConnectedConsole()
    await person.click(screen.getByRole('button', { name: 'Keyboard' }))

    fireEvent.pointerDown(screen.getByRole('button', { name: 'Esc' }))
    fireEvent.pointerDown(screen.getByRole('button', { name: 'Up' }))

    expect(rfb.keys).toEqual([
      { keysym: 0xff1b, code: 'Escape' },
      { keysym: 0xff52, code: 'ArrowUp' },
    ])
    // The press must not steal focus: a blur dismisses the on-screen keyboard.
    expect(document.activeElement).toBe(keyboardField())
  })

  it('hides the key strip until the keyboard is asked for', async () => {
    await renderConnectedConsole()
    expect(screen.queryByRole('group', { name: 'Keys' })).toBeNull()
  })
})
