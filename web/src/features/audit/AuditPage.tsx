import { Fragment, useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api, requestBlob } from '@/api/client'
import type { AuditEntry } from '@/api/types'
import { absoluteTime, relativeTime } from '@/lib/format'

const RANGES = [
  { value: '1h', label: 'Last hour' },
  { value: '24h', label: 'Last 24 hours' },
  { value: '7d', label: 'Last 7 days' },
  { value: '30d', label: 'Last 30 days' },
  { value: '', label: 'All time' },
]

export function AuditPage() {
  const [params, setParams] = useSearchParams()
  const [expanded, setExpanded] = useState<number | null>(null)
  const [exporting, setExporting] = useState(false)

  async function downloadExport(search: URLSearchParams) {
    setExporting(true)
    try {
      const blob = await requestBlob(`/audit-logs/export?${search}`)
      const url = URL.createObjectURL(blob)
      const link = document.createElement('a')
      link.href = url
      link.download = `audit-${new Date().toISOString().slice(0, 10)}.csv`
      link.click()
      URL.revokeObjectURL(url)
    } finally {
      setExporting(false)
    }
  }

  const filters = {
    range: params.get('range') ?? '24h',
    category: params.get('category') ?? '',
    action: params.get('action') ?? '',
    actor: params.get('actor') ?? '',
    outcome: params.get('outcome') ?? '',
    q: params.get('q') ?? '',
  }

  function setFilter(key: string, value: string) {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    setParams(next, { replace: true })
  }

  const categories = useQuery({
    queryKey: ['audit-categories'],
    queryFn: () => api.get<{ data: Record<string, string[]> }>('/audit-logs/categories'),
    staleTime: 5 * 60_000,
  })

  const query = new URLSearchParams({ per_page: '100' })
  for (const [key, value] of Object.entries(filters)) {
    if (value) query.set(key, value)
  }

  const entries = useQuery({
    queryKey: ['audit', filters],
    queryFn: () => api.get<{ data: AuditEntry[] }>(`/audit-logs?${query}`),
  })

  const actions = filters.category ? (categories.data?.data[filters.category] ?? []) : []

  return (
    <div className="space-y-5">
      <div className="flex items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Audit log</h1>
          <p className="text-sm text-muted">
            Every action that changed something, and every one that was refused.
          </p>
        </div>
        {/* Export follows the filters on screen, so what is downloaded is what
            was being looked at. It goes through the API client rather than a
            plain link because the access token lives in memory, and a browser
            navigation would arrive unauthenticated. */}
        <button
          onClick={() => void downloadExport(query)}
          disabled={exporting}
          className="rounded-md border border-border px-3 py-2 text-sm hover:bg-surface-raised disabled:opacity-40"
        >
          {exporting ? 'Preparing…' : 'Export CSV'}
        </button>
      </div>

      <div className="grid gap-3 rounded-lg border border-border p-3 sm:grid-cols-2 lg:grid-cols-6">
        <Select
          label="Time"
          value={filters.range}
          onChange={(v) => setFilter('range', v)}
          options={RANGES}
        />
        <Select
          label="Category"
          value={filters.category}
          onChange={(v) => {
            setFilter('category', v)
            setFilter('action', '') // an action from another category matches nothing
          }}
          options={[
            { value: '', label: 'All' },
            ...Object.keys(categories.data?.data ?? {}).map((c) => ({ value: c, label: c })),
          ]}
        />
        <Select
          label="Action"
          value={filters.action}
          onChange={(v) => setFilter('action', v)}
          options={[
            { value: '', label: filters.category ? 'All' : 'Pick a category first' },
            ...actions.map((a) => ({ value: a, label: a })),
          ]}
          disabled={!filters.category}
        />
        <Text label="Actor" value={filters.actor} onChange={(v) => setFilter('actor', v)} />
        <Select
          label="Outcome"
          value={filters.outcome}
          onChange={(v) => setFilter('outcome', v)}
          options={[
            { value: '', label: 'All' },
            { value: 'success', label: 'success' },
            { value: 'failure', label: 'failure' },
            { value: 'denied', label: 'denied' },
          ]}
        />
        <Text label="Search" value={filters.q} onChange={(v) => setFilter('q', v)} />
      </div>

      {entries.isLoading && <p className="text-sm text-muted">Loading…</p>}
      {entries.error && <p className="text-sm text-danger">Could not load the audit log.</p>}

      {entries.data && (
        <div className="overflow-hidden rounded-lg border border-border bg-surface-raised">
          <table className="w-full text-sm">
            <thead className="bg-surface-raised text-left text-xs uppercase tracking-wide text-muted">
              <tr>
                <th className="px-4 py-2 font-medium">When</th>
                <th className="px-4 py-2 font-medium">Actor</th>
                <th className="px-4 py-2 font-medium">Action</th>
                <th className="px-4 py-2 font-medium">Target</th>
                <th className="px-4 py-2 font-medium">Outcome</th>
              </tr>
            </thead>
            <tbody>
              {entries.data.data.length === 0 && (
                <tr>
                  <td colSpan={5} className="px-4 py-6 text-center text-muted">
                    Nothing matches these filters.
                  </td>
                </tr>
              )}
              {entries.data.data.map((entry) => (
                <Fragment key={entry.id}>
                  <tr
                    onClick={() => setExpanded(expanded === entry.id ? null : entry.id)}
                    className="cursor-pointer border-t border-border hover:bg-surface-raised"
                  >
                    <td className="px-4 py-2 text-muted" title={absoluteTime(entry.ts)}>
                      {relativeTime(entry.ts)}
                    </td>
                    <td className="px-4 py-2">
                      {entry.actor_name || <span className="text-muted">system</span>}
                      {entry.source_ip && (
                        <span className="block text-xs text-muted">{entry.source_ip}</span>
                      )}
                    </td>
                    <td className="px-4 py-2">
                      <span className="font-mono text-xs">{entry.action}</span>
                      <span className="block text-xs text-muted">{entry.category}</span>
                    </td>
                    <td className="px-4 py-2">
                      {entry.target_name || entry.target_id ? (
                        <>
                          <span>{entry.target_name || '—'}</span>
                          <span className="block font-mono text-[10px] text-muted">
                            {entry.target_type} {entry.target_id?.slice(0, 8)}
                          </span>
                        </>
                      ) : (
                        <span className="text-muted">—</span>
                      )}
                    </td>
                    <td className="px-4 py-2">
                      <OutcomeBadge outcome={entry.outcome} />
                    </td>
                  </tr>
                  {expanded === entry.id && (
                    <tr className="border-t border-border bg-surface-raised">
                      <td colSpan={5} className="px-4 py-3">
                        <div className="space-y-2 text-xs">
                          <div className="text-muted">
                            {absoluteTime(entry.ts)}
                            {entry.request_id && ` · request ${entry.request_id}`}
                          </div>
                          {entry.user_agent && <div className="text-muted">{entry.user_agent}</div>}
                          <pre className="overflow-x-auto rounded-md bg-surface p-2 font-mono">
                            {JSON.stringify(entry.details ?? {}, null, 2)}
                          </pre>
                        </div>
                      </td>
                    </tr>
                  )}
                </Fragment>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  )
}

function OutcomeBadge({ outcome }: { outcome: string }) {
  const tone =
    outcome === 'success'
      ? 'bg-state-running/15 text-state-running'
      : outcome === 'denied'
        ? 'bg-state-paused/15 text-state-paused'
        : 'bg-state-stopped/15 text-state-stopped'
  return <span className={`rounded-full px-2 py-0.5 text-xs ${tone}`}>{outcome}</span>
}

function Select({
  label,
  value,
  onChange,
  options,
  disabled,
}: {
  label: string
  value: string
  onChange: (v: string) => void
  options: { value: string; label: string }[]
  disabled?: boolean
}) {
  return (
    <label className="space-y-1 text-xs">
      <span className="block uppercase tracking-wide text-muted">{label}</span>
      <select
        value={value}
        disabled={disabled}
        onChange={(e) => onChange(e.target.value)}
        className="w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm disabled:opacity-50"
      >
        {options.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  )
}

function Text({
  label,
  value,
  onChange,
}: {
  label: string
  value: string
  onChange: (v: string) => void
}) {
  const [draft, setDraft] = useState(value)
  return (
    <label className="space-y-1 text-xs">
      <span className="block uppercase tracking-wide text-muted">{label}</span>
      <input
        value={draft}
        onChange={(e) => setDraft(e.target.value)}
        // Committed on blur or Enter rather than per keystroke: each change is
        // a query, and the audit table is the largest in the database.
        onBlur={() => onChange(draft)}
        onKeyDown={(e) => e.key === 'Enter' && onChange(draft)}
        className="w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm"
      />
    </label>
  )
}
