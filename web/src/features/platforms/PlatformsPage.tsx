import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { ConnectorInfo, Platform } from '@/api/types'
import { relativeTime } from '@/lib/format'
import { PlatformForm } from './PlatformForm'
import { PlatformDetail } from './PlatformDetail'

export function PlatformsPage() {
  const [editing, setEditing] = useState<Platform | 'new' | null>(null)
  const [selected, setSelected] = useState<Platform | null>(null)
  const queryClient = useQueryClient()

  const platforms = useQuery({
    queryKey: ['platforms'],
    queryFn: () => api.get<{ data: Platform[] }>('/platforms'),
    refetchInterval: 30_000,
  })

  const connectors = useQuery({
    queryKey: ['connectors'],
    queryFn: () => api.get<{ data: ConnectorInfo[] }>('/connectors'),
    staleTime: Infinity, // the set of compiled-in connectors cannot change at runtime
  })

  const toggle = useMutation({
    mutationFn: (platform: Platform) =>
      api.put<Platform>(`/platforms/${platform.id}`, {
        name: platform.name,
        type: platform.type,
        endpoint_url: platform.endpoint_url,
        datacenter: platform.datacenter,
        tls_mode: platform.tls_mode,
        is_enabled: !platform.is_enabled,
      }),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['platforms'] }),
  })

  if (platforms.isLoading) return <p className="text-sm text-muted">Loading…</p>

  const rows = platforms.data?.data ?? []

  return (
    <div className="space-y-5">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-xl font-semibold">Platforms</h1>
          <p className="text-sm text-muted">
            Clusters this portal synchronizes. Everything shown here is read from the platform.
          </p>
        </div>
        <button
          onClick={() => setEditing('new')}
          className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white"
        >
          Add platform
        </button>
      </div>

      {rows.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border p-8 text-center">
          <p className="text-sm text-muted">
            No platforms yet. Add one to start synchronizing an estate.
          </p>
        </div>
      ) : (
        <div className="overflow-hidden rounded-lg border border-border">
          <table className="w-full text-sm">
            <thead className="bg-surface-raised text-left text-xs uppercase tracking-wide text-muted">
              <tr>
                <th className="px-4 py-2 font-medium">Name</th>
                <th className="px-4 py-2 font-medium">Type</th>
                <th className="px-4 py-2 font-medium">Endpoint</th>
                <th className="px-4 py-2 font-medium">Health</th>
                <th className="px-4 py-2 font-medium">Last seen</th>
                <th className="px-4 py-2 font-medium">Enabled</th>
                <th className="px-4 py-2" />
              </tr>
            </thead>
            <tbody>
              {rows.map((platform) => (
                <tr key={platform.id} className="border-t border-border">
                  <td className="px-4 py-2">
                    <button
                      onClick={() => setSelected(platform)}
                      className="font-medium hover:text-accent hover:underline"
                    >
                      {platform.name}
                    </button>
                    <div className="text-xs text-muted">{platform.datacenter}</div>
                  </td>
                  <td className="px-4 py-2">
                    {platform.type}
                    {platform.detected_version && (
                      <span className="text-muted"> {platform.detected_version}</span>
                    )}
                  </td>
                  <td className="px-4 py-2 font-mono text-xs">{platform.endpoint_url}</td>
                  <td className="px-4 py-2">
                    <HealthBadge platform={platform} />
                  </td>
                  <td className="px-4 py-2 text-muted">
                    {platform.last_seen_at ? relativeTime(platform.last_seen_at) : 'never'}
                  </td>
                  <td className="px-4 py-2">
                    <button
                      onClick={() => toggle.mutate(platform)}
                      disabled={toggle.isPending}
                      className={`rounded-full px-2 py-0.5 text-xs ${
                        platform.is_enabled
                          ? 'bg-state-running/15 text-state-running'
                          : 'bg-state-stopped/15 text-state-stopped'
                      }`}
                    >
                      {platform.is_enabled ? 'enabled' : 'disabled'}
                    </button>
                  </td>
                  <td className="px-4 py-2 text-right">
                    <button
                      onClick={() => setEditing(platform)}
                      className="text-xs text-accent hover:underline"
                    >
                      Edit
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {editing && (
        <PlatformForm
          platform={editing === 'new' ? null : editing}
          connectors={connectors.data?.data ?? []}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null)
            void queryClient.invalidateQueries({ queryKey: ['platforms'] })
          }}
        />
      )}

      {selected && <PlatformDetail platform={selected} onClose={() => setSelected(null)} />}
    </div>
  )
}

function HealthBadge({ platform }: { platform: Platform }) {
  // A tripped breaker is not the same as an unhealthy platform, and hiding the
  // difference makes "why has nothing synced" unanswerable from this page.
  if (platform.breaker_open) {
    return (
      <span
        className="rounded-full bg-state-stopped/15 px-2 py-0.5 text-xs text-state-stopped"
        title="Repeated failures suspended synchronization; it retries automatically."
      >
        breaker open
      </span>
    )
  }
  const tone =
    platform.health === 'healthy'
      ? 'bg-state-running/15 text-state-running'
      : platform.health === 'degraded'
        ? 'bg-state-paused/15 text-state-paused'
        : 'bg-state-stopped/15 text-state-stopped'
  return (
    <span className={`rounded-full px-2 py-0.5 text-xs ${tone}`} title={platform.health_detail}>
      {platform.health}
    </span>
  )
}
