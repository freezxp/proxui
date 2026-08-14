import { api } from '@/api/client'
import type { MetricPoint, MetricSeriesResponse } from '@/api/types'

export const RANGES = ['1h', '6h', '24h', '7d', '30d', '1y'] as const
export type Range = (typeof RANGES)[number]

export const RANGE_LABELS: Record<Range, string> = {
  '1h': 'Last hour',
  '6h': '6 hours',
  '24h': '24 hours',
  '7d': '7 days',
  '30d': '30 days',
  '1y': '1 year',
}

/** The server picks the stored resolution; the client only asks for a window
 *  (docs/03-frs.md PERF-02). */
export function fetchMetrics(vmId: string, range: Range) {
  return api.get<MetricSeriesResponse>(`/vms/${vmId}/metrics?range=${range}`)
}

/** uPlot wants seconds, and columns rather than rows. */
export function toColumns(points: MetricPoint[]) {
  return {
    timestamps: points.map((p) => new Date(p.t).getTime() / 1000),
    cpu: points.map((p) => p.cpu_pct),
    memUsed: points.map((p) => p.mem_used_bytes),
    memPct: points.map((p) =>
      p.mem_total_bytes > 0 ? (p.mem_used_bytes / p.mem_total_bytes) * 100 : null,
    ),
    netRx: points.map((p) => p.net_rx_bps),
    netTx: points.map((p) => p.net_tx_bps),
    diskRead: points.map((p) => p.disk_read_bps),
    diskWrite: points.map((p) => p.disk_write_bps),
  }
}
