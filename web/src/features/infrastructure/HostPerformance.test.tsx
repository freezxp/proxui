import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'

/**
 * A node's performance charts.
 *
 * uPlot draws to a canvas that jsdom cannot render, so the assertions are about
 * the wiring around the chart — the range the request asks for, the honesty
 * label on the resolution, and the empty state — not the pixels.
 */

const apiGet = vi.fn()
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: { get: (...a: unknown[]) => apiGet(...a), post: vi.fn(), put: vi.fn(), del: vi.fn() },
  }
})

import { HostPerformance } from './HostPerformance'

function renderPerf() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return render(
    <QueryClientProvider client={client}>
      <HostPerformance hostId="host-1" />
    </QueryClientProvider>,
  )
}

const tempHistory = (labels: string[], n: number) => ({
  data: labels.map((label, li) => ({
    chip: 'coretemp-isa-0000',
    label,
    crit: 100,
    points: Array.from({ length: n }, (_, i) => ({
      t: new Date(1_700_000_000_000 + i * 60_000).toISOString(),
      v: 40 + li * 5 + i,
    })),
  })),
})

const seriesResponse = (resolution: string, n: number) => ({
  host_id: 'host-1',
  series: {
    resolution,
    bucket_seconds: 60,
    points: Array.from({ length: n }, (_, i) => ({
      t: new Date(1_700_000_000_000 + i * 60_000).toISOString(),
      cpu_pct: 30 + i,
      mem_used_bytes: 8 * 2 ** 30,
      mem_total_bytes: 16 * 2 ** 30,
      disk_read_bps: 0,
      disk_write_bps: 0,
      net_rx_bps: 0,
      net_tx_bps: 0,
      disk_used_bytes: 0,
    })),
  },
})

beforeEach(() => apiGet.mockReset())

describe('host performance', () => {
  it('requests both metrics and temperature history for the window', async () => {
    apiGet.mockImplementation((path: string) =>
      Promise.resolve(
        path?.includes('/sensors/history')
          ? tempHistory(['Package id 0'], 3)
          : seriesResponse('raw', 3),
      ),
    )
    renderPerf()

    await waitFor(() => expect(apiGet).toHaveBeenCalledWith('/hosts/host-1/metrics?range=6h'))
    await waitFor(() =>
      expect(apiGet).toHaveBeenCalledWith('/hosts/host-1/sensors/history?range=6h'),
    )
    expect(await screen.findByText(/1-minute samples/)).toBeTruthy()
    expect(screen.getByText('CPU')).toBeTruthy()
    expect(screen.getByText('Memory')).toBeTruthy()
    expect(screen.getByText('Memory used')).toBeTruthy()
    // Temperature is charted when the node has reported any.
    expect(screen.getByText('Temperature')).toBeTruthy()
  })

  it('caps the overlaid sensor lines and says how many are hidden', async () => {
    const many = Array.from({ length: 11 }, (_, i) => `Core ${i}`)
    apiGet.mockImplementation((path: string) =>
      Promise.resolve(
        path?.includes('/sensors/history') ? tempHistory(many, 4) : seriesResponse('raw', 4),
      ),
    )
    renderPerf()
    // 8-colour palette, 11 sensors -> the surplus is disclosed, not dropped silently.
    expect(await screen.findByText(/Showing the first 8 of 11 sensors/)).toBeTruthy()
  })

  it('re-requests when the range changes, and names averaged data as averaged', async () => {
    apiGet.mockImplementation((path: string) => {
      if (path?.includes('/sensors/history')) return Promise.resolve({ data: [] })
      return Promise.resolve(seriesResponse(path?.includes('30d') ? '5m' : 'raw', 5))
    })
    renderPerf()
    await screen.findByText('CPU')

    await userEvent.click(screen.getByText('30 days'))
    await waitFor(() => expect(apiGet).toHaveBeenCalledWith('/hosts/host-1/metrics?range=30d'))
    // A wide window comes back as 5-minute averages (no coarser host rollup),
    // and the label must say so rather than imply per-minute precision.
    expect(await screen.findByText(/5m averages/)).toBeTruthy()
  })

  it('says so when a window has no samples', async () => {
    apiGet.mockImplementation((path: string) =>
      Promise.resolve(path?.includes('/sensors/history') ? { data: [] } : seriesResponse('raw', 0)),
    )
    renderPerf()
    expect(await screen.findByText(/No samples in this window yet/)).toBeTruthy()
  })
})
