import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ReadinessSection } from './Readiness'

const get = vi.fn()
const post = vi.fn()
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: { get: (...a: unknown[]) => get(...a), post: (...a: unknown[]) => post(...a) },
  }
})

function readiness(overrides: Record<string, unknown> = {}) {
  return {
    portal_key: true,
    privileges: {
      missing: [],
      provisioning_available: true,
      missing_provisioning: [],
      template_build_available: false,
      missing_template: ['VM.Config.HWType'],
    },
    nodes: [
      {
        node: 'cx1',
        address: '10.0.0.2',
        reachable: true,
        prerequisites: [
          {
            id: 'lm-sensors',
            name: 'lm-sensors',
            needed: 'reading node temperatures',
            present: true,
            installable: true,
            packages: ['lm-sensors'],
            command: 'apt-get install -y lm-sensors',
          },
          {
            id: 'libguestfs-tools',
            name: 'libguestfs-tools',
            needed: 'installing a guest agent into a template',
            present: false,
            installable: true,
            packages: ['libguestfs-tools'],
            command: 'apt-get install -y libguestfs-tools',
          },
        ],
      },
      {
        node: 'pve2',
        address: '10.0.0.3',
        reachable: false,
        problem: "the node refused the portal's key; install the portal's public key",
        prerequisites: [],
      },
    ],
    ...overrides,
  }
}

function renderSection() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <ReadinessSection platformID="p1" />
    </QueryClientProvider>,
  )
}

describe('ReadinessSection', () => {
  beforeEach(() => {
    get.mockReset().mockResolvedValue(readiness())
    post.mockReset().mockResolvedValue({ state: 'running' })
  })

  // Checking costs an SSH handshake per node for an answer that changes about
  // once a year, so it must not happen because a drawer was opened.
  it('asks nothing until the button is pressed', async () => {
    const user = userEvent.setup()
    renderSection()

    expect(get).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: 'Check' }))
    await waitFor(() => expect(get).toHaveBeenCalledWith('/platforms/p1/readiness'))
  })

  // The failure that started all this: one node has the tool and another does
  // not, and nothing said so.
  it('shows what one node is missing and offers to install it', async () => {
    const user = userEvent.setup()
    renderSection()
    await user.click(screen.getByRole('button', { name: 'Check' }))

    await waitFor(() => screen.getByText(/libguestfs-tools/))
    expect(screen.getByRole('button', { name: 'Install' })).toBeTruthy()
  })

  // Unknown is not the same as missing: a node that would not let the portal in
  // has said nothing about what it has, and listing absences would send an
  // operator to fix the wrong thing.
  it('reports an unreachable node as unreachable, not as empty', async () => {
    const user = userEvent.setup()
    renderSection()
    await user.click(screen.getByRole('button', { name: 'Check' }))

    await waitFor(() => screen.getByText(/refused the portal's key/))
    // Only cx1's two prerequisites are listed; pve2 contributes none.
    expect(screen.getAllByText(/lm-sensors/)).toHaveLength(1)
  })

  // The portal is asking to run something as root on a hypervisor. It says
  // exactly what, and sends an identifier rather than a command.
  it('shows the command before running it, and posts an identifier', async () => {
    const user = userEvent.setup()
    renderSection()
    await user.click(screen.getByRole('button', { name: 'Check' }))
    await waitFor(() => screen.getByRole('button', { name: 'Install' }))

    await user.click(screen.getByRole('button', { name: 'Install' }))
    expect(screen.getByText('apt-get install -y libguestfs-tools')).toBeTruthy()
    expect(post).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: 'Install on cx1' }))
    await waitFor(() => expect(post).toHaveBeenCalled())
    const [path, body] = post.mock.calls[0] as [string, Record<string, unknown>]
    expect(path).toBe('/platforms/p1/nodes/cx1/install')
    expect(body).toEqual({ prerequisite: 'libguestfs-tools' })
  })

  // Widening a token is a decision made on the cluster. A portal that could
  // widen its own credential would have no limits worth reporting.
  it('names missing privileges without offering to grant them', async () => {
    const user = userEvent.setup()
    renderSection()
    await user.click(screen.getByRole('button', { name: 'Check' }))

    await waitFor(() => screen.getByText(/VM.Config.HWType/))
    expect(screen.getByText(/Datacenter → Permissions/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: /grant/i })).toBeNull()
  })

  it('points at the SSH key page when the portal has none', async () => {
    get.mockResolvedValue(readiness({ portal_key: false }))
    const user = userEvent.setup()
    renderSection()
    await user.click(screen.getByRole('button', { name: 'Check' }))

    await waitFor(() => screen.getByText(/Settings → SSH key/))
  })
})
