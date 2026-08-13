import { useQuery } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { Dashboard } from '@/api/types'

const HEALTH_STYLES: Record<string, string> = {
  healthy: 'text-running',
  degraded: 'text-paused',
  unreachable: 'text-danger',
  unknown: 'text-muted',
}

export function DashboardPage() {
  const { data, isLoading, error } = useQuery({
    queryKey: ['dashboard'],
    queryFn: () => api.get<Dashboard>('/dashboard'),
    refetchInterval: 30_000,
  })

  if (isLoading) return <p className="text-sm text-muted">Loading…</p>
  if (error) return <p className="text-sm text-danger">Could not load the dashboard.</p>
  if (!data) return null

  return (
    <div className="space-y-6">
      <h1 className="text-xl font-semibold">Dashboard</h1>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
        <Stat label="Virtual machines" value={data.total_vms} />
        <Stat label="Running" value={data.running_vms} tone="text-running" />
        <Stat label="Stopped" value={data.stopped_vms} tone="text-stopped" />
        <Stat label="Missing" value={data.missing_vms} tone={data.missing_vms > 0 ? 'text-stale' : undefined} />
      </div>

      <section className="space-y-2">
        <h2 className="text-sm font-medium text-muted">Platforms</h2>
        <div className="overflow-hidden rounded-lg border border-border">
          <table className="w-full text-sm">
            <thead className="bg-surface-raised text-left text-xs uppercase tracking-wide text-muted">
              <tr>
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">Datacenter</th>
                <th className="px-4 py-2 font-medium">Health</th>
                <th className="px-4 py-2 font-medium">Version</th>
                <th className="px-4 py-2 text-right font-medium">VMs</th>
              </tr>
            </thead>
            <tbody>
              {data.platforms.map((platform) => (
                <tr key={platform.id} className="border-t border-border">
                  <td className="px-4 py-2 font-medium">{platform.name}</td>
                  <td className="px-4 py-2 text-muted">{platform.datacenter}</td>
                  <td className={`px-4 py-2 ${HEALTH_STYLES[platform.health] ?? 'text-muted'}`}>
                    {platform.health}
                    {platform.breaker_open && <span className="ml-2 text-xs text-stale">(backing off)</span>}
                  </td>
                  <td className="px-4 py-2 text-muted">{platform.version ?? '—'}</td>
                  <td className="px-4 py-2 text-right tabular-nums">{platform.vm_count}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <div className="grid gap-6 lg:grid-cols-2">
        <TopList title="Busiest by CPU" unit="%" entries={data.top_cpu} />
        <TopList title="Busiest by memory" unit="%" entries={data.top_memory} />
      </div>
    </div>
  )
}

function Stat({ label, value, tone }: { label: string; value: number; tone?: string }) {
  return (
    <div className="rounded-lg border border-border bg-surface-raised p-4">
      <div className="text-xs uppercase tracking-wide text-muted">{label}</div>
      <div className={`mt-1 text-2xl font-semibold tabular-nums ${tone ?? ''}`}>{value}</div>
    </div>
  )
}

function TopList({
  title,
  unit,
  entries,
}: {
  title: string
  unit: string
  entries: { vm_id: string; name: string; platform_name: string; value: number }[]
}) {
  return (
    <section className="space-y-2">
      <h2 className="text-sm font-medium text-muted">{title}</h2>
      <div className="rounded-lg border border-border bg-surface-raised">
        {entries.length === 0 && <p className="p-4 text-sm text-muted">No recent samples.</p>}
        <ul className="divide-y divide-border">
          {entries.map((entry) => (
            <li key={entry.vm_id} className="flex items-center justify-between gap-4 px-4 py-2 text-sm">
              <span className="truncate">
                {entry.name} <span className="text-muted">· {entry.platform_name}</span>
              </span>
              <span className="tabular-nums">
                {entry.value.toFixed(1)}
                {unit}
              </span>
            </li>
          ))}
        </ul>
      </div>
    </section>
  )
}
