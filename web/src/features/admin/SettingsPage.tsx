import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { Setting } from '@/api/types'

export function SettingsPage() {
  const queryClient = useQueryClient()
  const [drafts, setDrafts] = useState<Record<string, number>>({})
  const [error, setError] = useState('')

  const settings = useQuery({
    queryKey: ['settings'],
    queryFn: () => api.get<{ data: Setting[] }>('/settings'),
  })

  const save = useMutation({
    mutationFn: ({ key, value }: { key: string; value: number | null }) =>
      api.put(`/settings/${key}`, { value }),
    onSuccess: (_, { key }) => {
      setError('')
      setDrafts((prev) => {
        const next = { ...prev }
        delete next[key]
        return next
      })
      void queryClient.invalidateQueries({ queryKey: ['settings'] })
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not save the setting.'),
  })

  if (settings.isLoading) return <p className="text-sm text-muted">Loading…</p>

  const groups = new Map<string, Setting[]>()
  for (const item of settings.data?.data ?? []) {
    groups.set(item.group, [...(groups.get(item.group) ?? []), item])
  }

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-xl font-semibold">Settings</h1>
        <p className="text-sm text-muted">
          Changes apply immediately; nothing here needs a restart.
        </p>
      </div>

      {error && <p className="text-sm text-danger">{error}</p>}

      {[...groups.entries()].map(([group, items]) => (
        <section key={group} className="space-y-3 rounded-lg border border-border p-4">
          <h2 className="text-sm font-medium">{group}</h2>
          <div className="space-y-4">
            {items.map((item) => {
              const draft = drafts[item.key] ?? item.value
              const dirty = draft !== item.value
              return (
                <div key={item.key} className="space-y-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <label htmlFor={item.key} className="min-w-56 text-sm">
                      {item.label}
                      {/* Saying a value differs from the default is what makes
                          a settings page auditable at a glance. */}
                      {item.modified && (
                        <span className="ml-2 rounded-full bg-accent/10 px-2 py-0.5 text-xs text-accent">
                          modified
                        </span>
                      )}
                    </label>
                    <input
                      id={item.key}
                      type="number"
                      value={draft}
                      min={item.min}
                      max={item.max}
                      onChange={(e) =>
                        setDrafts((prev) => ({ ...prev, [item.key]: Number(e.target.value) }))
                      }
                      className="w-32 rounded-md border border-border bg-surface px-3 py-1.5 text-sm"
                    />
                    <span className="text-xs text-muted">{unitLabel(item)}</span>
                    {dirty && (
                      <button
                        onClick={() => save.mutate({ key: item.key, value: draft })}
                        className="rounded-md bg-accent px-3 py-1.5 text-xs font-medium text-white"
                      >
                        Save
                      </button>
                    )}
                    {item.modified && !dirty && (
                      <button
                        onClick={() => save.mutate({ key: item.key, value: null })}
                        className="text-xs text-muted hover:underline"
                      >
                        Reset to {formatValue(item, item.default)}
                      </button>
                    )}
                  </div>
                  <p className="text-xs text-muted">
                    {item.help} Allowed: {formatValue(item, item.min)} to{' '}
                    {formatValue(item, item.max)}.
                  </p>
                </div>
              )
            })}
          </div>
        </section>
      ))}
    </div>
  )
}

function unitLabel(item: Setting): string {
  if (item.kind === 'days') return 'days'
  if (item.kind === 'count') return 'attempts'
  return 'seconds'
}

// Ranges are written in whatever unit reads naturally: "2592000 seconds" tells
// nobody it is a month.
function formatValue(item: Setting, value: number): string {
  if (item.kind === 'days') return `${value} days`
  if (item.kind === 'count') return String(value)
  if (value >= 86400) return `${Math.round(value / 86400)} days`
  if (value >= 3600) return `${Math.round(value / 3600)} hours`
  if (value >= 60) return `${Math.round(value / 60)} minutes`
  return `${value} seconds`
}
