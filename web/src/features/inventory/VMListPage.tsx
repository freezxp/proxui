import { Link, useSearchParams } from 'react-router-dom'
import { keepPreviousData, useQuery } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { Paged, PlatformHealth, VMListItem } from '@/api/types'
import { StateBadge } from '@/components/StateBadge'
import { bytes, percent, relativeTime, absoluteTime, uptime } from '@/lib/format'
import { useLiveInventory } from './useLiveInventory'

const PER_PAGE = 50

const STATES = ['running', 'stopped', 'paused', 'suspended', 'unknown'] as const

export function VMListPage() {
  // Filters live in the URL so a filtered view can be bookmarked, shared in a
  // ticket, and survives a reload - the states an operator actually returns to.
  const [params, setParams] = useSearchParams()
  const { connected } = useLiveInventory()

  const query = params.get('q') ?? ''
  const state = params.get('state') ?? ''
  const platform = params.get('platform_id') ?? ''
  const sort = params.get('sort') ?? 'name'
  const page = Math.max(1, Number(params.get('page') ?? '1') || 1)

  function update(changes: Record<string, string>) {
    const next = new URLSearchParams(params)
    for (const [key, value] of Object.entries(changes)) {
      if (value) next.set(key, value)
      else next.delete(key)
    }
    // Any filter change returns to the first page: staying on page 4 of a
    // result set that now has two pages shows an empty table.
    if (!('page' in changes)) next.delete('page')
    setParams(next, { replace: true })
  }

  const platforms = useQuery({
    queryKey: ['platforms'],
    queryFn: () => api.get<Paged<PlatformHealth>>('/platforms'),
    staleTime: 60_000,
  })

  const search = new URLSearchParams({ per_page: String(PER_PAGE), page: String(page), sort })
  if (query) search.set('q', query)
  if (state) search.set('state', state)
  if (platform) search.set('platform_id', platform)

  const vms = useQuery({
    queryKey: ['vms', search.toString()],
    queryFn: () => api.get<Paged<VMListItem>>(`/vms?${search}`),
    // Keeping the previous page visible while the next loads stops the table
    // collapsing to a spinner on every keystroke.
    placeholderData: keepPreviousData,
    refetchInterval: 60_000,
  })

  const total = vms.data?.meta.total ?? 0
  const pages = Math.max(1, Math.ceil(total / PER_PAGE))

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-xl font-semibold">Inventory</h1>
        <span className="flex items-center gap-2 text-xs text-muted">
          <span className={`h-2 w-2 rounded-full ${connected ? 'bg-running' : 'bg-stopped'}`} />
          {connected ? 'Live' : 'Reconnecting…'}
        </span>
      </div>

      <div className="flex flex-wrap gap-2">
        <input
          value={query}
          onChange={(e) => update({ q: e.target.value })}
          placeholder="Search by name"
          className="min-w-48 flex-1 rounded-md border border-border bg-surface-raised px-3 py-2 text-sm outline-none focus:border-accent"
        />
        <select
          value={state}
          onChange={(e) => update({ state: e.target.value })}
          className="rounded-md border border-border bg-surface-raised px-3 py-2 text-sm outline-none focus:border-accent"
        >
          <option value="">All states</option>
          {STATES.map((s) => (
            <option key={s} value={s}>
              {s}
            </option>
          ))}
        </select>
        <select
          value={platform}
          onChange={(e) => update({ platform_id: e.target.value })}
          className="rounded-md border border-border bg-surface-raised px-3 py-2 text-sm outline-none focus:border-accent"
        >
          <option value="">All platforms</option>
          {(platforms.data?.data ?? []).map((p) => (
            <option key={p.id} value={p.id}>
              {p.name}
            </option>
          ))}
        </select>
        {(query || state || platform) && (
          <button
            onClick={() => setParams(new URLSearchParams(), { replace: true })}
            className="rounded-md border border-border px-3 py-2 text-sm hover:bg-surface-raised"
          >
            Clear
          </button>
        )}
      </div>

      <div className="overflow-x-auto rounded-lg border border-border bg-surface-raised">
        <table className="w-full min-w-[52rem] text-sm">
          <thead className="bg-surface-raised text-left text-xs uppercase tracking-wide text-muted">
            <tr>
              <SortableHeader
                label="Name"
                field="name"
                sort={sort}
                onSort={(s) => update({ sort: s })}
              />
              <SortableHeader
                label="State"
                field="state"
                sort={sort}
                onSort={(s) => update({ sort: s })}
              />
              <th className="px-4 py-2 font-medium">Node</th>
              <SortableHeader
                label="vCPU"
                field="cpu"
                sort={sort}
                onSort={(s) => update({ sort: s })}
                align="right"
              />
              <SortableHeader
                label="Memory"
                field="memory"
                sort={sort}
                onSort={(s) => update({ sort: s })}
                align="right"
              />
              <th className="px-4 py-2 text-right font-medium">CPU</th>
              <th className="px-4 py-2 text-right font-medium">Mem</th>
              <SortableHeader
                label="Uptime"
                field="uptime"
                sort={sort}
                onSort={(s) => update({ sort: s })}
                align="right"
              />
              <th className="px-4 py-2 font-medium">Addresses</th>
            </tr>
          </thead>
          <tbody>
            {vms.isLoading && (
              <tr>
                <td colSpan={9} className="px-4 py-8 text-center text-muted">
                  Loading…
                </td>
              </tr>
            )}
            {vms.isError && (
              <tr>
                <td colSpan={9} className="px-4 py-8 text-center text-danger">
                  Could not load the inventory.
                </td>
              </tr>
            )}
            {vms.data?.data.length === 0 && (
              <tr>
                <td colSpan={9} className="px-4 py-8 text-center text-muted">
                  {query || state || platform
                    ? 'No virtual machines match these filters.'
                    : 'No virtual machines are visible to your account.'}
                </td>
              </tr>
            )}
            {vms.data?.data.map((vm) => (
              <tr key={vm.id} className="border-t border-border hover:bg-surface-raised/60">
                <td className="px-4 py-2">
                  <Link
                    to={`/vms/${vm.id}`}
                    className="font-medium hover:text-accent hover:underline"
                  >
                    {vm.name}
                  </Link>
                  <div className="text-xs text-muted">
                    {vm.vm_type} · {vm.external_id} · {vm.platform_name}
                  </div>
                </td>
                <td className="px-4 py-2">
                  <StateBadge state={vm.state} stale={vm.sync_state === 'missing'} />
                </td>
                <td className="px-4 py-2 text-muted">{vm.host_name ?? '—'}</td>
                <td className="px-4 py-2 text-right tabular-nums">{vm.cpu_cores || '—'}</td>
                <td className="px-4 py-2 text-right tabular-nums">{bytes(vm.memory_bytes)}</td>
                <td className="px-4 py-2 text-right tabular-nums">
                  {vm.state === 'running' ? percent(vm.cpu_pct) : '—'}
                </td>
                <td className="px-4 py-2 text-right tabular-nums">
                  {vm.state === 'running' && vm.mem_pct > 0 ? percent(vm.mem_pct) : '—'}
                </td>
                <td
                  className="px-4 py-2 text-right tabular-nums text-muted"
                  title={absoluteTime(vm.last_seen_at)}
                >
                  {vm.state === 'running' ? uptime(vm.uptime_s) : relativeTime(vm.last_seen_at)}
                </td>
                <td className="px-4 py-2 text-xs text-muted">
                  {vm.ip_addresses.length > 0 ? vm.ip_addresses.join(', ') : '—'}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <div className="flex items-center justify-between text-sm text-muted">
        <span>
          {total} virtual machine{total === 1 ? '' : 's'}
          {pages > 1 && ` · page ${page} of ${pages}`}
        </span>
        {pages > 1 && (
          <div className="flex gap-2">
            <button
              disabled={page <= 1}
              onClick={() => update({ page: String(page - 1) })}
              className="rounded-md border border-border px-3 py-1.5 disabled:opacity-40"
            >
              Previous
            </button>
            <button
              disabled={page >= pages}
              onClick={() => update({ page: String(page + 1) })}
              className="rounded-md border border-border px-3 py-1.5 disabled:opacity-40"
            >
              Next
            </button>
          </div>
        )}
      </div>
    </div>
  )
}

function SortableHeader({
  label,
  field,
  sort,
  onSort,
  align = 'left',
}: {
  label: string
  field: string
  sort: string
  onSort: (sort: string) => void
  align?: 'left' | 'right'
}) {
  const active = sort === field || sort === `-${field}`
  const descending = sort === `-${field}`

  return (
    <th className={`px-4 py-2 font-medium ${align === 'right' ? 'text-right' : ''}`}>
      <button
        onClick={() => onSort(descending ? field : `-${field}`)}
        className={`inline-flex items-center gap-1 uppercase tracking-wide ${active ? 'text-accent' : ''}`}
      >
        {label}
        {active && <span aria-hidden>{descending ? '↓' : '↑'}</span>}
      </button>
    </th>
  )
}
