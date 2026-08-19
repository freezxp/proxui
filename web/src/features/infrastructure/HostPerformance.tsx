import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { MetricSeriesResponse, SensorSeries } from '@/api/types'
import { MetricChart, type Series } from '@/components/MetricChart'
import { toColumns, RANGES, RANGE_LABELS, type Range } from '../inventory/metrics'
import { bytes, percent } from '@/lib/format'
import { shortChip } from './NodeSensors'

function themeColor(name: string): string {
  const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return value ? `rgb(${value})` : '#2563eb'
}

// Distinct hues for overlaid sensor lines. Fixed rather than theme tokens: a
// node can report a dozen sensors, more than the palette has semantic slots,
// and these only ever sit on a chart surface where contrast is stable.
const SENSOR_COLORS = [
  '#ef4444',
  '#f59e0b',
  '#10b981',
  '#3b82f6',
  '#8b5cf6',
  '#ec4899',
  '#14b8a6',
  '#f97316',
]

const degrees = (v: number) => `${Math.round(v)}°C`

// alignSensors turns per-sensor series into one shared timeline. A node writes
// all its readings at the same instant each pass, so the union of timestamps is
// just the sorted set; a sensor that appeared partway through the window is
// null before it did, which uPlot draws as a gap rather than a drop to zero.
function alignSensors(series: SensorSeries[]): { timestamps: number[]; lines: Series[] } {
  const times = new Set<number>()
  for (const s of series) for (const p of s.points) times.add(new Date(p.t).getTime() / 1000)
  const timestamps = [...times].sort((a, b) => a - b)
  const index = new Map(timestamps.map((t, i) => [t, i]))

  const lines: Series[] = series.slice(0, SENSOR_COLORS.length).map((s, i) => {
    const values: (number | null)[] = timestamps.map(() => null)
    for (const p of s.points) values[index.get(new Date(p.t).getTime() / 1000)!] = p.v
    return {
      label: `${shortChip(s.chip)} ${s.label}`,
      color: SENSOR_COLORS[i],
      values,
      format: degrees,
    }
  })
  return { timestamps, lines }
}

/**
 * A node's performance history: CPU, memory, and temperature.
 *
 * Nodes report only CPU and memory through the platform API — there is no
 * per-node disk or network series to draw — so those two are a smaller board
 * than a VM's. Temperature is different in kind: it comes from the node itself
 * over SSH (ADR 0007), one line per sensor, overlaid so the CPU package and the
 * NVMe can be compared on one axis.
 */
export function HostPerformance({ hostId }: { hostId: string }) {
  const [range, setRange] = useState<Range>('6h')
  const polls = range === '1h' || range === '6h' ? 60_000 : false

  const metrics = useQuery({
    queryKey: ['host-metrics', hostId, range],
    queryFn: () => api.get<MetricSeriesResponse>(`/hosts/${hostId}/metrics?range=${range}`),
    refetchInterval: polls,
  })
  const temps = useQuery({
    queryKey: ['host-temp-history', hostId, range],
    queryFn: () =>
      api.get<{ data: SensorSeries[] }>(`/hosts/${hostId}/sensors/history?range=${range}`),
    refetchInterval: polls,
  })

  const points = metrics.data?.series?.points ?? []
  const columns = toColumns(points)
  const resolution = metrics.data?.series?.resolution

  const temp = alignSensors(temps.data?.data ?? [])
  const hiddenSensors = Math.max(0, (temps.data?.data?.length ?? 0) - SENSOR_COLORS.length)

  const cpu: Series[] = [
    { label: 'CPU', color: themeColor('--accent'), values: columns.cpu, format: percent },
  ]
  const memPct: Series[] = [
    {
      label: 'Memory',
      color: themeColor('--state-running'),
      values: columns.memPct,
      format: percent,
    },
  ]
  const memUsed: Series[] = [
    {
      label: 'Memory used',
      color: themeColor('--state-running'),
      values: columns.memUsed,
      format: bytes,
    },
  ]

  const loading = metrics.isLoading || temps.isLoading
  const failed = metrics.error && temps.error
  const nothing = points.length === 0 && temp.lines.length === 0

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="flex flex-wrap gap-1">
          {RANGES.map((option) => (
            <button
              key={option}
              onClick={() => setRange(option)}
              className={`rounded px-2 py-1 text-xs ${
                range === option
                  ? 'bg-accent text-white'
                  : 'border border-border hover:bg-surface-raised'
              }`}
            >
              {RANGE_LABELS[option]}
            </button>
          ))}
        </div>
        {resolution && (
          <span className="text-xs text-muted">
            {points.length} points ·{' '}
            {resolution === 'raw' ? '1-minute samples' : `${resolution} averages`}
          </span>
        )}
      </div>

      {loading && <p className="text-sm text-muted">Loading…</p>}
      {failed && <p className="text-sm text-danger">Could not load metrics.</p>}
      {!loading && !failed && nothing && (
        <p className="text-sm text-muted">No samples in this window yet.</p>
      )}

      {!loading && !failed && !nothing && (
        <div className="grid gap-3 md:grid-cols-2">
          {points.length > 0 && (
            <>
              <HostChartCard title="CPU">
                <MetricChart
                  timestamps={columns.timestamps}
                  series={cpu}
                  percentScale
                  height={160}
                />
              </HostChartCard>
              <HostChartCard title="Memory">
                <MetricChart
                  timestamps={columns.timestamps}
                  series={memPct}
                  percentScale
                  height={160}
                />
              </HostChartCard>
              <HostChartCard title="Memory used">
                <MetricChart timestamps={columns.timestamps} series={memUsed} height={160} />
              </HostChartCard>
            </>
          )}
          {temp.lines.length > 0 && (
            <div className="md:col-span-2">
              <HostChartCard title="Temperature">
                <MetricChart timestamps={temp.timestamps} series={temp.lines} height={200} />
                {hiddenSensors > 0 && (
                  <p className="text-xs text-muted">
                    Showing the first {SENSOR_COLORS.length} of {temps.data?.data?.length} sensors.
                  </p>
                )}
              </HostChartCard>
            </div>
          )}
        </div>
      )}
    </div>
  )
}

function HostChartCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="space-y-1.5 rounded-lg border border-border bg-surface-raised p-3">
      <h4 className="text-xs font-medium text-muted">{title}</h4>
      {children}
    </section>
  )
}
