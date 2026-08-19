import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

/**
 * The setup guide on a platform.
 *
 * What it has to get right is the state an operator is actually in: nodes that
 * are not answering, and the reason each one is not. A guide that shows the
 * steps without showing which node still needs them is a document, and there
 * is already a document.
 */

const apiGet = vi.fn()

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: {
      get: (...args: unknown[]) => apiGet(...args),
      post: vi.fn(),
      put: vi.fn(),
      del: vi.fn(),
    },
  }
})

const copied: string[] = []
vi.mock('@/lib/clipboard', () => ({
  copyText: (text: string) => {
    copied.push(text)
    return Promise.resolve(true)
  },
}))

import { MonitoringGuide } from './MonitoringGuide'

const platform = { id: 'plat-1', name: 'pve-home' } as never

const node = (over: Record<string, unknown>) => ({
  id: 'host-1',
  platform_id: 'plat-1',
  name: 'pve1',
  platform_name: 'pve-home',
  status: 'online',
  cpu_cores: 8,
  memory_bytes: 1,
  version: '8.2.2',
  uptime_s: 1,
  sync_state: 'ok',
  vm_count: 3,
  sensors_ever_read: false,
  ...over,
})

function renderGuide() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <MonitoringGuide platform={platform} />
    </QueryClientProvider>,
  )
}

function mockApi(
  hosts: unknown[],
  key: Record<string, unknown> = { exists: true, public_key: 'ssh-ed25519 AAAAC3Nz portal' },
) {
  apiGet.mockImplementation((path: string) => {
    if (path === '/ssh-key') return Promise.resolve(key)
    return Promise.resolve({ data: hosts })
  })
}

beforeEach(() => {
  apiGet.mockReset()
  copied.length = 0
})

describe('monitoring guide', () => {
  it('shows only this platform’s nodes', async () => {
    mockApi([node({}), node({ id: 'host-2', name: 'other', platform_id: 'plat-2' })])
    renderGuide()

    expect(await screen.findByText('pve1')).toBeTruthy()
    expect(screen.queryByText('other')).toBeNull()
  })

  it('names why a node is not answering, per node', async () => {
    mockApi([
      node({ sensors_ever_read: true, sensor_error: "the node refused the portal's key" }),
      node({ id: 'host-2', name: 'pve2' }),
    ])
    renderGuide()

    expect(await screen.findByText("the node refused the portal's key")).toBeTruthy()
    // Never tried and tried-and-refused need different things done about them.
    expect(screen.getByText('not read yet')).toBeTruthy()
  })

  it('puts the portal’s real key in the command to paste', async () => {
    mockApi([node({})])
    renderGuide()

    const buttons = await screen.findAllByRole('button', { name: 'Copy' })
    await userEvent.click(buttons[1])

    expect(copied[0]).toContain('ssh-ed25519 AAAAC3Nz portal')
    expect(copied[0]).toContain('authorized_keys')
  })

  it('sends the operator to generate a key when the portal has none', async () => {
    mockApi([node({})], { exists: false })
    renderGuide()

    expect(await screen.findByText(/This portal has no SSH key yet/)).toBeTruthy()
  })

  it('drops the setup steps once every node answers', async () => {
    mockApi([
      node({
        sensors_ever_read: true,
        sensors: {
          count: 7,
          chips: ['coretemp-isa-0000'],
          hottest: { chip: 'coretemp-isa-0000', label: 'Package id 0', kind: 'temp_c', value: 47 },
        },
      }),
    ])
    renderGuide()

    expect(await screen.findByText('7 sensors')).toBeTruthy()
    expect(screen.queryByText(/Install lm-sensors/)).toBeNull()
  })
})
