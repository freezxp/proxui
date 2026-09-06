import { useMemo, useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { ContainerApp, ContainerAppUpstream, ContainerDeployment } from '@/api/types'
import { relativeTime } from '@/lib/format'
import { DeployDialog } from './DeployDialog'
import { DeploymentLog } from './DeploymentLog'

/** Container apps: install an application into an LXC from the shipped
 *  catalogue (APP-01…APP-06, ADR 0012).
 *
 *  Distinct from Published apps, which are Cloudflare hostnames and do not run
 *  anything. This page installs software; that one exposes it.
 */
export function ContainerAppsPage() {
  const [query, setQuery] = useState('')
  const [tag, setTag] = useState('')
  const [chosen, setChosen] = useState<ContainerApp | null>(null)
  const [watching, setWatching] = useState('')

  const catalogue = useQuery({
    queryKey: ['container-apps'],
    queryFn: () =>
      api.get<{ data: ContainerApp[]; tags: string[]; upstream: ContainerAppUpstream }>(
        '/container-apps',
      ),
    // Ships with the build; it cannot go stale within a session.
    staleTime: Infinity,
  })

  const deployments = useQuery({
    queryKey: ['container-deployments'],
    queryFn: () => api.get<{ data: ContainerDeployment[] }>('/container-deployments'),
    // Something in flight is worth watching; a settled list is not.
    refetchInterval: (q) =>
      q.state.data?.data.some((d) => d.state === 'pending' || d.state === 'deploying')
        ? 10_000
        : false,
  })

  const apps = catalogue.data?.data ?? []
  const tags = catalogue.data?.tags ?? []
  const upstream = catalogue.data?.upstream

  // Filtered here rather than by asking the server again: the whole catalogue
  // arrived in one response and is a few hundred kilobytes.
  const shown = useMemo(() => {
    const needle = query.trim().toLowerCase()
    return apps.filter(
      (a) =>
        (tag === '' || (a.tags ?? []).includes(tag)) &&
        (needle === '' ||
          a.name.toLowerCase().includes(needle) ||
          a.id.includes(needle) ||
          (a.tags ?? []).some((t) => t.includes(needle))),
    )
  }, [apps, query, tag])

  return (
    <div className="space-y-5">
      <header className="flex flex-wrap items-baseline justify-between gap-3">
        <div>
          <h1 className="text-lg font-semibold">Container apps</h1>
          <p className="text-sm text-muted">
            Install an application into a new LXC container. These are the community Proxmox VE
            Helper-Scripts, vendored with this portal at a reviewed commit and run on the node you
            pick.
          </p>
        </div>
        {upstream && (
          <p
            className="font-mono text-[11px] text-muted"
            title="the commits this catalogue came from"
          >
            {upstream.scripts_repo}@{upstream.scripts_ref.slice(0, 7)} · {upstream.engine_repo}@
            {upstream.engine_ref.slice(0, 7)}
          </p>
        )}
      </header>

      {(deployments.data?.data.length ?? 0) > 0 && (
        <section className="rounded-md border border-border bg-surface-raised">
          <h2 className="border-b border-border px-3 py-2 text-sm font-medium">
            Recent deployments
          </h2>
          <ul className="divide-y divide-border">
            {(deployments.data?.data ?? []).slice(0, 8).map((d) => (
              <li key={d.id} className="flex items-center justify-between gap-3 px-3 py-2 text-sm">
                <div className="min-w-0">
                  <span className="font-medium">{d.app_name}</span>
                  <span className="text-muted"> on {d.node}</span>
                  {d.ctid && <span className="ml-2 font-mono text-xs text-muted">CT {d.ctid}</span>}
                  <span className="block text-xs text-muted">
                    {d.requested_by ? `${d.requested_by}, ` : ''}
                    {relativeTime(d.created_at)}
                  </span>
                </div>
                <div className="flex shrink-0 items-center gap-3">
                  <StateLabel state={d.state} />
                  <button
                    onClick={() => setWatching(d.id)}
                    className="rounded-md border border-border px-2 py-1 text-xs"
                  >
                    Log
                  </button>
                </div>
              </li>
            ))}
          </ul>
        </section>
      )}

      <div className="flex flex-wrap gap-2">
        <input
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={`Search ${apps.length} applications…`}
          className="min-w-56 flex-1 rounded-md border border-border bg-surface-raised px-3 py-2 text-sm"
        />
        <select
          value={tag}
          onChange={(e) => setTag(e.target.value)}
          className="rounded-md border border-border bg-surface-raised px-2 py-2 text-sm"
        >
          <option value="">All categories</option>
          {tags.map((t) => (
            <option key={t} value={t}>
              {t}
            </option>
          ))}
        </select>
      </div>

      {catalogue.isLoading ? (
        <p className="text-sm text-muted">Loading…</p>
      ) : shown.length === 0 ? (
        <p className="text-sm text-muted">Nothing matches that.</p>
      ) : (
        <ul className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
          {shown.map((app) => (
            <li key={app.id}>
              <button
                onClick={() => setChosen(app)}
                className="flex h-full w-full flex-col gap-1 rounded-md border border-border bg-surface-raised p-3 text-left hover:border-accent"
              >
                <span className="text-sm font-medium">{app.name}</span>
                <span className="text-xs text-muted">
                  {[
                    app.cores && `${app.cores} core${app.cores > 1 ? 's' : ''}`,
                    app.memory_mb && `${app.memory_mb} MB`,
                    app.disk_gb && `${app.disk_gb} GB`,
                  ]
                    .filter(Boolean)
                    .join(' · ') || 'defaults chosen by the script'}
                </span>
                {(app.tags?.length ?? 0) > 0 && (
                  <span className="mt-auto flex flex-wrap gap-1 pt-1">
                    {app.tags?.slice(0, 3).map((t) => (
                      <span
                        key={t}
                        className="rounded-sm bg-accent-wash px-1.5 py-0.5 text-[10px] text-accent-strong"
                      >
                        {t}
                      </span>
                    ))}
                  </span>
                )}
              </button>
            </li>
          ))}
        </ul>
      )}

      {chosen && (
        <DeployDialog
          app={chosen}
          upstream={upstream}
          onClose={() => setChosen(null)}
          onStarted={setWatching}
        />
      )}
      {watching && <DeploymentLog id={watching} onClose={() => setWatching('')} />}
    </div>
  )
}

function StateLabel({ state }: { state: ContainerDeployment['state'] }) {
  const tone =
    state === 'failed' ? 'text-danger' : state === 'ready' ? 'text-running' : 'text-muted'
  return <span className={`text-xs ${tone}`}>{state === 'deploying' ? 'installing…' : state}</span>
}
