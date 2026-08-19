import { Fragment, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/api/client'
import { NodeSensorPanel, TempCell } from './NodeSensors'
import { HostPerformance } from './HostPerformance'
import type { HostRow, NetworkRow, StorageRow } from '@/api/types'
import { bytes, uptime } from '@/lib/format'

/** Hosts, storage and networks are read-only: every field is synced from the
 *  platform and the portal owns none of it, so these are tables rather than
 *  editors (INV-05). */

export function HostsPage() {
  const hosts = useQuery({
    queryKey: ['hosts'],
    queryFn: () => api.get<{ data: HostRow[] }>('/hosts'),
    refetchInterval: 60_000,
  })
  // Which node's sensors are open. One at a time: the panel is wide, and
  // comparing two nodes at a glance is what the temperature column is for.
  const [open, setOpen] = useState<string | null>(null)

  return (
    <Page
      title="Hosts"
      subtitle="The nodes your VMs run on."
      loading={hosts.isLoading}
      empty={(hosts.data?.data.length ?? 0) === 0}
      columns={['Host', 'Platform', 'Status', 'Capacity', 'VMs', 'Uptime', 'Temp']}
    >
      {hosts.data?.data.map((host) => (
        <Fragment key={host.id}>
          <tr
            className="cursor-pointer border-t border-border hover:bg-surface-raised"
            onClick={() => setOpen(open === host.id ? null : host.id)}
          >
            <td className="px-4 py-2">
              <div className="font-medium">{host.name}</div>
              {host.version && <div className="text-xs text-muted">{host.version}</div>}
            </td>
            <td className="px-4 py-2 text-muted">{host.platform_name}</td>
            <td className="px-4 py-2">
              <StatusBadge status={host.status} stale={host.sync_state === 'missing'} />
            </td>
            <td className="px-4 py-2 text-muted">
              {host.cpu_cores > 0 && `${host.cpu_cores} cores`}
              {host.memory_bytes > 0 && ` · ${bytes(host.memory_bytes)}`}
            </td>
            <td className="px-4 py-2">{host.vm_count}</td>
            <td className="px-4 py-2 text-muted">
              {host.status === 'online' ? uptime(host.uptime_s) : '—'}
            </td>
            <td className="px-4 py-2">
              <TempCell summary={host.sensors} />
            </td>
          </tr>
          {open === host.id && (
            <tr className="border-t border-border">
              <td colSpan={7} className="space-y-4 bg-surface p-4">
                <HostPerformance hostId={host.id} />
                <NodeSensorPanel hostId={host.id} hostName={host.name} />
              </td>
            </tr>
          )}
        </Fragment>
      ))}
    </Page>
  )
}

export function StoragePage() {
  const storage = useQuery({
    queryKey: ['storage'],
    queryFn: () => api.get<{ data: StorageRow[] }>('/storage'),
    refetchInterval: 60_000,
  })

  return (
    <Page
      title="Storage"
      subtitle="Pools backing the estate, and how full they are."
      loading={storage.isLoading}
      empty={(storage.data?.data.length ?? 0) === 0}
      columns={['Pool', 'Platform', 'Type', 'Usage', 'Shared']}
    >
      {storage.data?.data.map((pool) => {
        const pct = pool.total_bytes > 0 ? (pool.used_bytes / pool.total_bytes) * 100 : 0
        return (
          <tr key={pool.id} className="border-t border-border">
            <td className="px-4 py-2">
              <div className="font-medium">{pool.name}</div>
              {pool.host_name && <div className="text-xs text-muted">{pool.host_name}</div>}
            </td>
            <td className="px-4 py-2 text-muted">{pool.platform_name}</td>
            <td className="px-4 py-2 text-muted">{pool.storage_type || '—'}</td>
            <td className="px-4 py-2">
              {pool.total_bytes > 0 ? (
                <div className="space-y-1">
                  <div className="text-xs">
                    {bytes(pool.used_bytes)} of {bytes(pool.total_bytes)} ({pct.toFixed(0)}%)
                  </div>
                  {/* Colour changes at the thresholds an operator acts on: 80%
                      is worth planning for, 90% is worth doing something about
                      today (docs/13-ui-design.md §13.2). */}
                  <div className="h-1.5 w-40 overflow-hidden rounded-full bg-border">
                    <div
                      className={`h-full ${
                        pct >= 90
                          ? 'bg-state-stopped'
                          : pct >= 80
                            ? 'bg-state-paused'
                            : 'bg-state-running'
                      }`}
                      style={{ width: `${Math.min(pct, 100)}%` }}
                    />
                  </div>
                </div>
              ) : (
                <span className="text-muted">unknown</span>
              )}
            </td>
            <td className="px-4 py-2 text-muted">{pool.is_shared ? 'yes' : 'no'}</td>
          </tr>
        )
      })}
    </Page>
  )
}

export function NetworksPage() {
  const networks = useQuery({
    queryKey: ['networks'],
    queryFn: () => api.get<{ data: NetworkRow[] }>('/networks'),
    refetchInterval: 60_000,
  })

  return (
    <Page
      title="Networks"
      subtitle="Interfaces and bridges, as the platform reports them."
      loading={networks.isLoading}
      empty={(networks.data?.data.length ?? 0) === 0}
      columns={['Interface', 'Host', 'Type', 'Address', 'VLAN']}
    >
      {networks.data?.data.map((net) => (
        <tr key={net.id} className="border-t border-border">
          <td className="px-4 py-2 font-medium">{net.name}</td>
          <td className="px-4 py-2 text-muted">{net.host_name || net.platform_name}</td>
          <td className="px-4 py-2 text-muted">{net.net_type || '—'}</td>
          <td className="px-4 py-2 font-mono text-xs">{net.cidr || '—'}</td>
          <td className="px-4 py-2 text-muted">{net.vlan_tag ?? '—'}</td>
        </tr>
      ))}
    </Page>
  )
}

function Page({
  title,
  subtitle,
  columns,
  loading,
  empty,
  children,
}: {
  title: string
  subtitle: string
  columns: string[]
  loading: boolean
  empty: boolean
  children: React.ReactNode
}) {
  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-xl font-semibold">{title}</h1>
        <p className="text-sm text-muted">{subtitle}</p>
      </div>

      {loading ? (
        <p className="text-sm text-muted">Loading…</p>
      ) : empty ? (
        <div className="rounded-lg border border-dashed border-border bg-surface-raised p-8 text-center text-sm text-muted">
          Nothing synced yet. Add a platform, or wait for the next synchronization.
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-border bg-surface-raised">
          <table className="w-full text-sm">
            <thead className="bg-surface-raised text-left text-xs uppercase tracking-wide text-muted">
              <tr>
                {columns.map((c) => (
                  <th key={c} className="px-4 py-2 font-medium">
                    {c}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>{children}</tbody>
          </table>
        </div>
      )}
    </div>
  )
}

function StatusBadge({ status, stale }: { status: string; stale: boolean }) {
  const tone =
    status === 'online'
      ? 'bg-state-running/15 text-state-running'
      : status === 'unknown'
        ? 'bg-surface-raised text-muted'
        : 'bg-state-stopped/15 text-state-stopped'
  return (
    <span className="flex items-center gap-1.5">
      <span className={`rounded-full px-2 py-0.5 text-xs ${tone}`}>{status}</span>
      {stale && (
        <span
          className="text-xs text-state-paused"
          title="The platform stopped reporting this host; it is kept until confirmed gone."
        >
          stale
        </span>
      )}
    </span>
  )
}
