import { useQuery } from '@tanstack/react-query'
import { MetricChart, type Series } from '@/components/MetricChart'
import { bytes, percent } from '@/lib/format'
import { fetchMetrics, toColumns, RANGES, RANGE_LABELS, type Range } from './metrics'

function themeColor(name: string): string {
  const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return value ? `rgb(${value})` : '#2563eb'
}

const perSecond = (v: number) => `${bytes(v)}/s`

export function PerformanceTab({
  vmId,
  range,
  onRangeChange,
}: {
  vmId: string
  range: Range
  onRangeChange: (range: Range) => void
}) {
  const metrics = useQuery({
    queryKey: ['vm-metrics', vmId, range],
    queryFn: () => fetchMetrics(vmId, range),
    // Only the short ranges are worth polling; a year-long chart does not
    // change meaningfully in a minute.
    refetchInterval: range === '1h' || range === '6h' ? 60_000 : false,
  })

  const points = metrics.data?.series.points ?? []
  const columns = toColumns(points)
  const resolution = metrics.data?.series.resolution

  const cpu: Series[] = [
    { label: 'CPU', color: themeColor('--accent'), values: columns.cpu, format: percent },
  ]
  const memory: Series[] = [
    {
      label: 'Memory',
      color: themeColor('--state-running'),
      values: columns.memUsed,
      format: bytes,
    },
  ]
  const network: Series[] = [
    { label: 'Receive', color: themeColor('--accent'), values: columns.netRx, format: perSecond },
    {
      label: 'Transmit',
      color: themeColor('--paused'),
      values: columns.netTx,
      format: perSecond,
    },
  ]
  const disk: Series[] = [
    { label: 'Read', color: themeColor('--accent'), values: columns.diskRead, format: perSecond },
    {
      label: 'Write',
      color: themeColor('--paused'),
      values: columns.diskWrite,
      format: perSecond,
    },
  ]

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-wrap gap-1">
          {RANGES.map((option) => (
            <button
              key={option}
              onClick={() => onRangeChange(option)}
              className={`rounded-md px-3 py-1.5 text-sm ${
                range === option
                  ? 'bg-accent text-white'
                  : 'border border-border hover:bg-surface-inset'
              }`}
            >
              {RANGE_LABELS[option]}
            </button>
          ))}
        </div>
        {/* Naming the resolution keeps the chart honest: three-hourly averages
            must not look like per-minute detail. */}
        {resolution && (
          <span className="text-xs text-muted">
            {points.length} points ·{' '}
            {resolution === 'raw' ? '1-minute samples' : `${resolution} averages`}
          </span>
        )}
      </div>

      {metrics.isLoading && <p className="text-sm text-muted">Loading…</p>}
      {metrics.error && <p className="text-sm text-danger">Could not load metrics.</p>}

      {!metrics.isLoading && !metrics.error && (
        <div className="space-y-5">
          <ChartCard title="CPU">
            <MetricChart timestamps={columns.timestamps} series={cpu} percentScale />
          </ChartCard>
          <ChartCard title="Memory used">
            <MetricChart timestamps={columns.timestamps} series={memory} />
          </ChartCard>
          <ChartCard title="Network">
            <MetricChart timestamps={columns.timestamps} series={network} />
          </ChartCard>
          <ChartCard title="Disk I/O">
            <MetricChart timestamps={columns.timestamps} series={disk} />
          </ChartCard>
        </div>
      )}
    </div>
  )
}

function ChartCard({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <section className="space-y-2 rounded-lg border border-border bg-surface-raised p-4">
      <h3 className="text-sm font-medium text-muted">{title}</h3>
      {children}
    </section>
  )
}
