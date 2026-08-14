import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { Platform, SyncRun } from '@/api/types'
import { Drawer } from '@/components/Drawer'
import { absoluteTime, relativeTime } from '@/lib/format'

export function PlatformDetail({ platform, onClose }: { platform: Platform; onClose: () => void }) {
  const queryClient = useQueryClient()
  const [confirmName, setConfirmName] = useState('')
  const [danger, setDanger] = useState(false)
  const [error, setError] = useState('')

  const runs = useQuery({
    queryKey: ['sync-runs', platform.id],
    queryFn: () => api.get<{ data: SyncRun[] }>(`/platforms/${platform.id}/sync-runs`),
    refetchInterval: 15_000,
  })

  const syncNow = useMutation({
    mutationFn: () => api.post(`/platforms/${platform.id}/sync`, {}),
    onSuccess: () => {
      // The run appears once the worker picks it up, so refetch shortly after
      // rather than pretending it is already there.
      setTimeout(
        () => void queryClient.invalidateQueries({ queryKey: ['sync-runs', platform.id] }),
        1500,
      )
    },
  })

  const remove = useMutation({
    mutationFn: () => api.del(`/platforms/${platform.id}`),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ['platforms'] })
      onClose()
    },
    onError: (err) =>
      setError(err instanceof Error ? err.message : 'Could not delete the platform.'),
  })

  return (
    <Drawer title={platform.name} onClose={onClose}>
      <div className="space-y-5">
        <dl className="grid grid-cols-2 gap-y-2 text-sm">
          <dt className="text-muted">Type</dt>
          <dd>
            {platform.type} {platform.detected_version}
          </dd>
          <dt className="text-muted">Endpoint</dt>
          <dd className="break-all font-mono text-xs">{platform.endpoint_url}</dd>
          <dt className="text-muted">Datacenter</dt>
          <dd>{platform.datacenter || '—'}</dd>
          <dt className="text-muted">Certificate</dt>
          <dd>{platform.tls_mode}</dd>
          <dt className="text-muted">Health</dt>
          <dd>
            {platform.health}
            {platform.health_detail && (
              <span className="block text-xs text-muted">{platform.health_detail}</span>
            )}
          </dd>
          <dt className="text-muted">Last seen</dt>
          <dd title={platform.last_seen_at && absoluteTime(platform.last_seen_at)}>
            {platform.last_seen_at ? relativeTime(platform.last_seen_at) : 'never'}
          </dd>
          <dt className="text-muted">Sync every</dt>
          <dd>
            inventory {platform.sync_intervals.inventory}s · metrics{' '}
            {platform.sync_intervals.metrics}s
          </dd>
        </dl>

        {platform.breaker_open && (
          <p className="rounded-md bg-state-paused/10 p-3 text-sm text-state-paused">
            Synchronization is suspended after repeated failures. It resumes automatically; a manual
            sync bypasses the wait.
          </p>
        )}

        <div>
          <div className="mb-2 flex items-center justify-between">
            <h3 className="text-sm font-medium">Recent synchronizations</h3>
            <button
              onClick={() => syncNow.mutate()}
              disabled={syncNow.isPending}
              className="rounded-md border border-border px-2 py-1 text-xs disabled:opacity-40"
            >
              {syncNow.isPending ? 'Queued…' : 'Sync now'}
            </button>
          </div>

          {runs.isLoading ? (
            <p className="text-sm text-muted">Loading…</p>
          ) : (runs.data?.data.length ?? 0) === 0 ? (
            <p className="text-sm text-muted">Nothing has run yet.</p>
          ) : (
            <div className="max-h-80 overflow-y-auto rounded-md border border-border">
              <table className="w-full text-xs">
                <thead className="sticky top-0 bg-surface-raised text-left text-muted">
                  <tr>
                    <th className="px-3 py-1.5 font-medium">When</th>
                    <th className="px-3 py-1.5 font-medium">Kind</th>
                    <th className="px-3 py-1.5 font-medium">Result</th>
                    <th className="px-3 py-1.5 font-medium">Took</th>
                  </tr>
                </thead>
                <tbody>
                  {runs.data?.data.map((run) => (
                    <tr key={run.id} className="border-t border-border align-top">
                      <td className="px-3 py-1.5 text-muted" title={absoluteTime(run.started_at)}>
                        {relativeTime(run.started_at)}
                        <span className="block text-[10px]">{run.trigger}</span>
                      </td>
                      <td className="px-3 py-1.5">{run.kind}</td>
                      <td className="px-3 py-1.5">
                        <span
                          className={
                            run.status === 'success'
                              ? 'text-state-running'
                              : run.status === 'partial'
                                ? 'text-state-paused'
                                : 'text-danger'
                          }
                        >
                          {run.status}
                        </span>
                        <span className="block text-muted">{summarize(run)}</span>
                        {run.error && <span className="block text-danger">{run.error}</span>}
                      </td>
                      <td className="px-3 py-1.5 text-muted">{Math.round(run.duration_ms)} ms</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>

        <div className="space-y-2 rounded-lg border border-danger/40 p-3">
          <h3 className="text-sm font-medium text-danger">Danger zone</h3>
          {!danger ? (
            <button onClick={() => setDanger(true)} className="text-sm text-danger hover:underline">
              Delete this platform
            </button>
          ) : (
            <div className="space-y-2">
              <p className="text-xs text-muted">
                Deleting removes this platform and every VM, host and metric synchronized from it.
                The audit trail is kept. Type <span className="font-mono">{platform.name}</span> to
                confirm.
              </p>
              <input
                value={confirmName}
                onChange={(e) => setConfirmName(e.target.value)}
                className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
                placeholder={platform.name}
              />
              <div className="flex gap-2">
                <button
                  onClick={() => remove.mutate()}
                  disabled={confirmName !== platform.name || remove.isPending}
                  className="rounded-md bg-danger px-3 py-1.5 text-sm text-white disabled:opacity-40"
                >
                  {remove.isPending ? 'Deleting…' : 'Delete permanently'}
                </button>
                <button
                  onClick={() => {
                    setDanger(false)
                    setConfirmName('')
                  }}
                  className="rounded-md border border-border px-3 py-1.5 text-sm"
                >
                  Cancel
                </button>
              </div>
              {error && <p className="text-xs text-danger">{error}</p>}
            </div>
          )}
        </div>
      </div>
    </Drawer>
  )
}

// A run's interesting numbers differ by kind, and zeros are noise: a sync that
// changed nothing should read as "no changes", not a row of zeroes.
function summarize(run: SyncRun): string {
  const parts = Object.entries(run.stats)
    .filter(([, value]) => value > 0)
    .map(([key, value]) => `${value} ${key}`)
  return parts.length > 0 ? parts.join(' · ') : 'no changes'
}
