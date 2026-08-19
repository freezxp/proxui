import { describe, expect, it, vi, beforeEach } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

/**
 * The hosts table's temperature column and the panel behind it.
 *
 * The case worth holding still is the empty one. Most nodes start with neither
 * the portal's key installed nor lm-sensors, so "no readings" is the normal
 * first state, and a panel that says so without saying what to do about it
 * would leave every operator stuck at the same place.
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

import { HostsPage } from './InfrastructurePage'

const hot = {
  id: 'host-1',
  name: 'pve1',
  platform_name: 'pve-home',
  status: 'online',
  cpu_cores: 8,
  memory_bytes: 68719476736,
  version: '8.2.2',
  uptime_s: 86400,
  sync_state: 'ok',
  vm_count: 12,
  sensors: {
    count: 3,
    chips: ['coretemp-isa-0000', 'nvme-pci-0100'],
    hottest: {
      chip: 'coretemp-isa-0000',
      label: 'Package id 0',
      kind: 'temp_c',
      value: 84,
      crit: 100,
    },
  },
}

const silent = { ...hot, id: 'host-2', name: 'pve2', sensors: undefined }

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <HostsPage />
    </QueryClientProvider>,
  )
}

beforeEach(() => {
  apiGet.mockReset()
})

describe('node temperature', () => {
  it('shows the hottest reading and names the sensor it came from', async () => {
    apiGet.mockResolvedValue({ data: [hot] })
    renderPage()

    expect(await screen.findByText('84°C')).toBeTruthy()
    // Which sensor, not just how hot: a package at 84°C and an NVMe at 84°C
    // are different afternoons.
    expect(screen.getByText('Package id 0')).toBeTruthy()
  })

  it('shows a dash for a node that has never answered, not a zero', async () => {
    apiGet.mockResolvedValue({ data: [silent] })
    renderPage()

    await screen.findByText('pve2')
    expect(screen.getByText('—')).toBeTruthy()
    expect(screen.queryByText('0°C')).toBeNull()
  })

  it('opens every sensor for a node when its row is clicked', async () => {
    apiGet.mockImplementation((path: string) => {
      if (path === '/hosts') return Promise.resolve({ data: [hot] })
      return Promise.resolve({
        host_id: 'host-1',
        at: '2026-08-19T02:00:00Z',
        readings: [
          {
            chip: 'coretemp-isa-0000',
            label: 'Package id 0',
            kind: 'temp_c',
            value: 84,
            crit: 100,
          },
          { chip: 'coretemp-isa-0000', label: 'Core 0', kind: 'temp_c', value: 81, crit: 100 },
          { chip: 'nvme-pci-0100', label: 'Composite', kind: 'temp_c', value: 38, crit: 85 },
        ],
        summary: hot.sensors,
        node: {
          address: '10.0.30.111',
          ssh_user: 'root',
          algorithm: 'ssh-ed25519',
          fingerprint: 'SHA256:abc',
          first_seen_at: '2026-08-19T01:00:00Z',
        },
      })
    })

    renderPage()
    await userEvent.click(await screen.findByText('pve1'))

    // Grouped by chip, with the bus suffix trimmed off the heading.
    expect(await screen.findByText('coretemp')).toBeTruthy()
    expect(screen.getByText('nvme')).toBeTruthy()
    expect(screen.getByText('Core 0')).toBeTruthy()
    expect(screen.getByText('Composite')).toBeTruthy()
    // The chip's own limit travels with the reading, because 84°C means
    // different things on different hardware.
    expect(screen.getAllByText('limit 100°C').length).toBe(2)
    expect(screen.getByText('limit 85°C')).toBeTruthy()
  })

  it('tells an operator what to install when a node has never answered', async () => {
    apiGet.mockImplementation((path: string) => {
      if (path === '/hosts') return Promise.resolve({ data: [silent] })
      return Promise.resolve({
        host_id: 'host-2',
        readings: [],
        summary: { count: 0, chips: [], hottest: {} },
        node: { last_error: "the node refused the portal's key" },
      })
    })

    renderPage()
    await userEvent.click(await screen.findByText('pve2'))

    // The reason first, then both halves of the fix.
    expect(await screen.findByText(/refused the portal's key/)).toBeTruthy()
    await waitFor(() => {
      expect(screen.getByText('/root/.ssh/authorized_keys')).toBeTruthy()
      expect(screen.getByText('apt install lm-sensors')).toBeTruthy()
    })
  })
})
