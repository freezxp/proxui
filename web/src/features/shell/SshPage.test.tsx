import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { ApiError } from '@/api/client'

/**
 * The SSH page's job before a terminal exists: collect a credential, and make
 * the two host-key answers legible. Those are the parts an operator has to get
 * right, and the parts that are pure UI rather than protocol.
 *
 * The terminal itself is stubbed. xterm.js measures glyphs against a real
 * canvas, which jsdom does not have; what it does with the bytes is covered
 * where the bytes are, in the Go bridge tests.
 */

const { FakeTerminal } = vi.hoisted(() => {
  class FakeTerminal {
    static last: FakeTerminal | null = null
    pasted: string[] = []
    selected = ''
    visible = 'load average: 0.14'
    everything = 'earlier output\nload average: 0.14'
    mouse = false
    constructor() {
      FakeTerminal.last = this
    }
    snapshot(scope: 'screen' | 'all') {
      return scope === 'screen' ? this.visible : this.everything
    }
    mouseReporting() {
      return this.mouse
    }
    paste(text: string) {
      this.pasted.push(text)
    }
    selection() {
      return this.selected
    }
    clearSelection() {
      this.selected = ''
    }
    clear() {}
    focus() {}
    fit() {}
  }
  return { FakeTerminal }
})

vi.mock('./SshTerminal', async () => {
  const react = await vi.importActual<typeof import('react')>('react')
  return {
    SshTerminal: react.forwardRef(function StubTerminal(
      props: { onOpen: () => void },
      ref: React.Ref<unknown>,
    ) {
      const instance = react.useMemo(() => new FakeTerminal(), [])
      react.useImperativeHandle(ref, () => instance)
      react.useEffect(() => props.onOpen(), [])
      return react.createElement('div', { 'data-testid': 'terminal' })
    }),
  }
})

const apiPost = vi.fn()
const apiGet = vi.fn()
const apiDel = vi.fn()

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: {
      get: (...args: unknown[]) => apiGet(...args),
      post: (...args: unknown[]) => apiPost(...args),
      del: (...args: unknown[]) => apiDel(...args),
      put: vi.fn(),
    },
  }
})

// Imported after the mocks so the page picks them up.
const { SshPage } = await import('./SshPage')

const RUNNING_VM = {
  id: 'vm1',
  name: 'web-01',
  state: 'running',
  platform_name: 'lab',
  host_name: 'pve1',
  ip_addresses: ['10.0.0.9', '10.0.0.10'],
}

const SESSION = {
  session_id: 'sess-1',
  ws_url: '/ws/ssh/ticket-1',
  expires_in: 60,
  address: '10.0.0.9:22',
  ssh_user: 'root',
  host_key: { algorithm: 'ssh-ed25519', fingerprint: 'SHA256:abc' },
  home: '/root',
  files_available: true,
}

function renderPage() {
  return render(
    <MemoryRouter initialEntries={['/ssh/vm1']}>
      <Routes>
        <Route path="/ssh/:vmId" element={<SshPage />} />
      </Routes>
    </MemoryRouter>,
  )
}

async function fillPassword(user: ReturnType<typeof userEvent.setup>, password: string) {
  await user.clear(screen.getByLabelText('Username'))
  await user.type(screen.getByLabelText('Username'), 'root')
  await user.type(screen.getByLabelText('Password'), password)
}

describe('SshPage', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    apiGet.mockResolvedValue(RUNNING_VM)
  })

  it('connects with what the operator typed and never asks the server to keep it', async () => {
    apiPost.mockResolvedValue(SESSION)
    const user = userEvent.setup()
    renderPage()

    await screen.findByText(/Connect over SSH/)
    await fillPassword(user, 'hunter2')
    await user.click(screen.getByRole('button', { name: 'Connect' }))

    await waitFor(() => expect(apiPost).toHaveBeenCalled())
    const [path, body] = apiPost.mock.calls[0]
    expect(path).toBe('/vms/vm1/ssh')
    expect(body).toMatchObject({ username: 'root', password: 'hunter2', private_key: '' })

    await screen.findByTestId('terminal')
    // Cleared from the form once it has been spent: the page has no further
    // use for it, and a React tree lives until the tab closes.
    expect(screen.queryByLabelText('Password')).toBeNull()
  })

  it('shows the fingerprint of an untrusted host and only connects once it is accepted', async () => {
    apiPost.mockRejectedValueOnce(
      new ApiError(409, 'ssh.host_key_unknown', 'not trusted', 'req-1', undefined, {
        body: { address: '10.0.0.9:22', algorithm: 'ssh-ed25519', fingerprint: 'SHA256:abcdef' },
      }),
    )
    const user = userEvent.setup()
    renderPage()

    await screen.findByText(/Connect over SSH/)
    await fillPassword(user, 'hunter2')
    await user.click(screen.getByRole('button', { name: 'Connect' }))

    await screen.findByText(/Trust this machine/)
    expect(screen.getByText('SHA256:abcdef')).toBeTruthy()

    apiPost.mockResolvedValueOnce(SESSION)
    await user.click(screen.getByRole('button', { name: 'Accept and connect' }))

    await waitFor(() => expect(apiPost).toHaveBeenCalledTimes(2))
    // The acceptance names the exact fingerprint that was shown, so a
    // different key arriving in between is still refused.
    expect(apiPost.mock.calls[1][1]).toMatchObject({ accept_host_key: 'SHA256:abcdef' })
  })

  it('refuses a changed host key without offering a way past it', async () => {
    apiPost.mockRejectedValue(
      new ApiError(409, 'ssh.host_key_mismatch', 'changed', 'req-2', undefined, {
        body: {
          address: '10.0.0.9:22',
          expected: 'SHA256:old',
          got: 'SHA256:new',
          first_seen_at: '2026-08-01T10:00:00Z',
        },
      }),
    )
    const user = userEvent.setup()
    renderPage()

    await screen.findByText(/Connect over SSH/)
    await fillPassword(user, 'hunter2')
    await user.click(screen.getByRole('button', { name: 'Connect' }))

    await screen.findByText(/The host key has changed/)
    expect(screen.getByText('SHA256:old')).toBeTruthy()
    expect(screen.getByText('SHA256:new')).toBeTruthy()
    // No accept button anywhere: clearing the pin is an administrator's job.
    expect(screen.queryByRole('button', { name: /Accept/ })).toBeNull()
  })

  it('says what went wrong when the guest rejects the credential', async () => {
    apiPost.mockRejectedValue(
      new ApiError(401, 'ssh.auth_failed', 'The guest rejected those credentials.'),
    )
    const user = userEvent.setup()
    renderPage()

    await screen.findByText(/Connect over SSH/)
    await fillPassword(user, 'wrong')
    await user.click(screen.getByRole('button', { name: 'Connect' }))

    await screen.findByText('The guest rejected those credentials.')
    // Still on the form, ready for another try.
    expect(screen.getByRole('button', { name: 'Connect' })).toBeTruthy()
  })

  it('offers the addresses the platform reported, and nothing else', async () => {
    renderPage()
    await screen.findByText(/Connect over SSH/)

    const options = screen.getAllByRole('option').map((o) => o.textContent)
    expect(options).toEqual(['Choose automatically', '10.0.0.9', '10.0.0.10'])
  })

  it('warns when the VM is not running', async () => {
    apiGet.mockResolvedValue({ ...RUNNING_VM, state: 'stopped' })
    renderPage()

    await screen.findByText(/This VM is stopped/)
  })

  it('falls back to a paste panel where the browser will not share the clipboard', async () => {
    apiPost.mockResolvedValue(SESSION)
    const user = userEvent.setup()
    // No navigator.clipboard at all, which is every plain-HTTP origin.
    Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true })

    renderPage()
    await screen.findByText(/Connect over SSH/)
    await fillPassword(user, 'hunter2')
    await user.click(screen.getByRole('button', { name: 'Connect' }))
    await screen.findByTestId('terminal')

    await user.click(screen.getByRole('button', { name: 'Paste' }))
    const panel = await screen.findByLabelText('Paste into the terminal')

    await user.type(panel, 'systemctl status')
    await user.click(screen.getByRole('button', { name: 'Send' }))

    expect(FakeTerminal.last?.pasted).toEqual(['systemctl status'])
  })

  // --- copying out of a program that has taken the mouse (SSH-08) ---------

  async function connected(user: ReturnType<typeof userEvent.setup>) {
    apiPost.mockResolvedValue(SESSION)
    renderPage()
    await screen.findByText(/Connect over SSH/)
    await fillPassword(user, 'hunter2')
    await user.click(screen.getByRole('button', { name: 'Connect' }))
    await screen.findByTestId('terminal')
  }

  it('offers the buffer as text when a drag cannot select', async () => {
    const user = userEvent.setup()
    await connected(user)
    // What tmux does on sight, and what makes dragging select nothing.
    FakeTerminal.last!.mouse = true

    await user.click(screen.getByRole('button', { name: 'Select' }))

    await screen.findByText(/A full-screen program has the mouse/)
    expect(screen.getByLabelText('Terminal text').textContent).toBe('load average: 0.14')

    await user.click(screen.getByRole('button', { name: 'With scrollback' }))
    expect(screen.getByLabelText('Terminal text').textContent).toBe(
      'earlier output\nload average: 0.14',
    )
  })

  it('opens the panel when Copy is pressed with nothing selected', async () => {
    const user = userEvent.setup()
    await connected(user)
    // Which is the state a drag leaves you in when the guest ate the drag.
    await user.click(screen.getByRole('button', { name: 'Copy' }))

    expect(screen.getByLabelText('Terminal text').textContent).toBe('load average: 0.14')
  })

  it('copies the whole snapshot without needing a selection', async () => {
    const user = userEvent.setup()
    // After setup(), which installs a clipboard stub of its own.
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    await connected(user)

    await user.click(screen.getByRole('button', { name: 'Select' }))
    await user.click(screen.getByRole('button', { name: 'Copy all' }))

    await waitFor(() => expect(writeText).toHaveBeenCalledWith('load average: 0.14'))
  })

  // --- the portal's own key (SSH-11..SSH-14, ADR 0006) --------------------

  // withPortalKey routes the two GETs the page makes: the VM, and where the
  // portal's key is installed on it.
  function withPortalKey(installs: Array<{ ssh_user: string; stale?: boolean }>) {
    apiGet.mockImplementation((path: string) => {
      if (path === '/vms/vm1/ssh-key') {
        return Promise.resolve({
          key_exists: true,
          fingerprint: 'SHA256:portal',
          data: installs.map((entry) => ({
            vm_id: 'vm1',
            vm_name: 'web-01',
            ssh_user: entry.ssh_user,
            fingerprint: 'SHA256:portal',
            installed_at: '2026-08-16T10:00:00Z',
            installed_by: 'user-1',
            stale: entry.stale ?? false,
          })),
        })
      }
      return Promise.resolve(RUNNING_VM)
    })
  }

  it('does not offer the portal key when the portal has not got one', async () => {
    apiGet.mockImplementation((path: string) =>
      path === '/vms/vm1/ssh-key'
        ? Promise.resolve({ key_exists: false, data: [] })
        : Promise.resolve(RUNNING_VM),
    )
    renderPage()

    await screen.findByText(/Connect over SSH/)
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith('/vms/vm1/ssh-key'))
    expect(screen.queryByRole('button', { name: 'Portal key' })).toBeNull()
  })

  it('connects with a boolean rather than a secret when the portal key is chosen', async () => {
    withPortalKey([{ ssh_user: 'root' }])
    apiPost.mockResolvedValue(SESSION)
    const user = userEvent.setup()
    renderPage()

    await screen.findByText(/Connect over SSH/)
    // Installed for the account in the form, so it is the default: this is the
    // entire point of the feature, no password typed at all.
    await waitFor(() => expect(screen.queryByLabelText('Password')).toBeNull())
    await user.click(screen.getByRole('button', { name: 'Connect' }))

    await waitFor(() => expect(apiPost).toHaveBeenCalled())
    const [path, body] = apiPost.mock.calls[0]
    expect(path).toBe('/vms/vm1/ssh')
    expect(body).toMatchObject({ username: 'root', use_portal_key: true, password: '' })
    expect(body.private_key).toBe('')
  })

  it('still asks for a password where the key is only installed for someone else', async () => {
    withPortalKey([{ ssh_user: 'deploy' }])
    renderPage()

    await screen.findByText(/Connect over SSH/)
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith('/vms/vm1/ssh-key'))
    // The choice is offered, because the key exists and could be installed —
    // but it is not preselected, because here it would simply fail.
    expect(screen.getByRole('button', { name: 'Portal key' })).toBeTruthy()
    expect(screen.getByLabelText('Password')).toBeTruthy()
  })

  it('installs the key over the session that is already open', async () => {
    withPortalKey([])
    apiPost.mockResolvedValue(SESSION)
    const user = userEvent.setup()
    renderPage()

    await screen.findByText(/Connect over SSH/)
    await fillPassword(user, 'hunter2')
    await user.click(screen.getByRole('button', { name: 'Connect' }))
    await screen.findByTestId('terminal')

    apiPost.mockResolvedValue({ vm_id: 'vm1', ssh_user: 'root', stale: false })
    await user.click(screen.getByRole('button', { name: 'Install portal key' }))

    await waitFor(() => expect(apiPost).toHaveBeenCalledWith('/ssh-sessions/sess-1/portal-key', {}))
    // And the page re-reads where the key now is, rather than assuming.
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith('/vms/vm1/ssh-key'))
  })

  it('offers removal where the key is already installed for this account', async () => {
    withPortalKey([{ ssh_user: 'root' }])
    apiPost.mockResolvedValue(SESSION)
    apiDel.mockResolvedValue(undefined)
    const user = userEvent.setup()
    renderPage()

    await screen.findByText(/Connect over SSH/)
    await user.click(screen.getByRole('button', { name: 'Connect' }))
    await screen.findByTestId('terminal')

    await user.click(screen.getByRole('button', { name: 'Remove portal key' }))
    await waitFor(() => expect(apiDel).toHaveBeenCalledWith('/ssh-sessions/sess-1/portal-key'))
  })

  it('closes the session on the server when the operator disconnects', async () => {
    apiPost.mockResolvedValue(SESSION)
    apiDel.mockResolvedValue(undefined)
    const user = userEvent.setup()
    renderPage()

    await screen.findByText(/Connect over SSH/)
    await fillPassword(user, 'hunter2')
    await user.click(screen.getByRole('button', { name: 'Connect' }))
    await screen.findByTestId('terminal')

    await user.click(screen.getByRole('button', { name: 'Disconnect' }))
    await waitFor(() => expect(apiDel).toHaveBeenCalledWith('/ssh-sessions/sess-1'))
  })
})
