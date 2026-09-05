import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { AlertRule, AlertStatus, VMGroup } from '@/api/types'
import { absoluteTime, relativeTime } from '@/lib/format'

const METRICS = [
  { value: 'cpu_pct', label: 'CPU', unit: '%' },
  { value: 'mem_pct', label: 'Memory', unit: '%' },
  { value: 'disk_read_bps', label: 'Disk read', unit: 'B/s' },
  { value: 'disk_write_bps', label: 'Disk write', unit: 'B/s' },
  { value: 'net_rx_bps', label: 'Network receive', unit: 'B/s' },
  { value: 'net_tx_bps', label: 'Network transmit', unit: 'B/s' },
]

const DURATIONS = [
  { value: 0, label: 'immediately' },
  { value: 300, label: 'for 5 minutes' },
  { value: 600, label: 'for 10 minutes' },
  { value: 1800, label: 'for 30 minutes' },
  { value: 3600, label: 'for an hour' },
]

const COOLDOWNS = [
  { value: 0, label: 'never repeat' },
  { value: 900, label: 'every 15 minutes' },
  { value: 1800, label: 'every 30 minutes' },
  { value: 3600, label: 'every hour' },
]

export function AlertsPanel() {
  const queryClient = useQueryClient()
  const [error, setError] = useState('')
  const [form, setForm] = useState({
    name: '',
    metric: 'cpu_pct',
    op: '>',
    threshold: 90,
    duration_s: 600,
    cooldown_s: 1800,
    severity: 'warning',
    vm_group_id: '',
  })

  const rules = useQuery({
    queryKey: ['alert-rules'],
    queryFn: () => api.get<{ data: AlertRule[] }>('/alert-rules'),
    refetchInterval: 30_000,
  })
  const firing = useQuery({
    queryKey: ['alerts'],
    queryFn: () => api.get<{ data: AlertStatus[]; meta: { total: number } }>('/alerts'),
    refetchInterval: 30_000,
  })
  const groups = useQuery({
    queryKey: ['vm-groups'],
    queryFn: () => api.get<{ data: VMGroup[] }>('/vm-groups'),
  })

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ['alert-rules'] })
    void queryClient.invalidateQueries({ queryKey: ['alerts'] })
  }

  const create = useMutation({
    mutationFn: () => api.post('/alert-rules', { ...form, vm_group_id: form.vm_group_id || null }),
    onSuccess: () => {
      setError('')
      setForm((prev) => ({ ...prev, name: '' }))
      invalidate()
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not add the rule.'),
  })

  const toggle = useMutation({
    mutationFn: (rule: AlertRule) =>
      api.put(`/alert-rules/${rule.id}`, { is_enabled: !rule.is_enabled }),
    onSuccess: invalidate,
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/alert-rules/${id}`),
    onSuccess: invalidate,
  })

  const unit = METRICS.find((m) => m.value === form.metric)?.unit ?? ''

  return (
    <div className="space-y-5">
      {/* Firing first: an operator opening this page wants to know what is
          wrong now, not what rules exist. */}
      <section className="space-y-2">
        <h2 className="text-sm font-medium">
          Firing now
          {(firing.data?.meta.total ?? 0) > 0 && (
            <span className="ml-2 rounded-full bg-danger/15 px-2 py-0.5 text-xs text-danger">
              {firing.data?.meta.total}
            </span>
          )}
        </h2>
        {(firing.data?.data.length ?? 0) === 0 ? (
          <p className="rounded-lg border border-border p-4 text-sm text-muted">
            Nothing is firing.
          </p>
        ) : (
          <div className="overflow-hidden rounded-lg border border-danger/40">
            <table className="w-full text-sm">
              <thead className="bg-danger/5 text-left text-xs uppercase tracking-wide text-muted">
                <tr>
                  <th className="px-4 py-2 font-medium">VM</th>
                  <th className="px-4 py-2 font-medium">Rule</th>
                  <th className="px-4 py-2 font-medium">Value</th>
                  <th className="px-4 py-2 font-medium">Since</th>
                </tr>
              </thead>
              <tbody>
                {firing.data?.data.map((status) => (
                  <tr key={`${status.rule_id}-${status.vm_id}`} className="border-t border-border">
                    <td className="px-4 py-2 font-medium">{status.vm_name}</td>
                    <td className="px-4 py-2">
                      {status.rule_name}
                      <span className="block text-xs text-muted">{status.severity}</span>
                    </td>
                    <td className="px-4 py-2">{formatValue(status.metric, status.last_value)}</td>
                    <td className="px-4 py-2 text-muted" title={absoluteTime(status.since)}>
                      {relativeTime(status.since)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>

      <section className="space-y-3 rounded-lg border border-border p-4">
        <h2 className="text-sm font-medium">New rule</h2>
        {/* Written as a sentence rather than a form grid: a threshold rule is
            one statement, and reading it back is how a mistake gets caught. */}
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <input
            value={form.name}
            onChange={(e) => setForm({ ...form, name: e.target.value })}
            placeholder="Rule name"
            className="rounded-md border border-border bg-surface px-3 py-1.5"
          />
          <span className="text-muted">alert when</span>
          <select
            value={form.metric}
            onChange={(e) => setForm({ ...form, metric: e.target.value })}
            className="rounded-md border border-border bg-surface px-2 py-1.5"
          >
            {METRICS.map((m) => (
              <option key={m.value} value={m.value}>
                {m.label}
              </option>
            ))}
          </select>
          <select
            value={form.op}
            onChange={(e) => setForm({ ...form, op: e.target.value })}
            className="rounded-md border border-border bg-surface px-2 py-1.5"
          >
            <option value=">">is above</option>
            <option value="<">is below</option>
          </select>
          <input
            type="number"
            value={form.threshold}
            onChange={(e) => setForm({ ...form, threshold: Number(e.target.value) })}
            className="w-24 rounded-md border border-border bg-surface px-2 py-1.5"
          />
          <span className="text-muted">{unit}</span>
          <select
            value={form.duration_s}
            onChange={(e) => setForm({ ...form, duration_s: Number(e.target.value) })}
            className="rounded-md border border-border bg-surface px-2 py-1.5"
          >
            {DURATIONS.map((d) => (
              <option key={d.value} value={d.value}>
                {d.label}
              </option>
            ))}
          </select>
          <span className="text-muted">on</span>
          <select
            value={form.vm_group_id}
            onChange={(e) => setForm({ ...form, vm_group_id: e.target.value })}
            className="rounded-md border border-border bg-surface px-2 py-1.5"
          >
            <option value="">every VM</option>
            {groups.data?.data.map((g) => (
              <option key={g.id} value={g.id}>
                {g.name}
              </option>
            ))}
          </select>
          <span className="text-muted">at</span>
          <select
            value={form.severity}
            onChange={(e) => setForm({ ...form, severity: e.target.value })}
            className="rounded-md border border-border bg-surface px-2 py-1.5"
          >
            <option value="info">info</option>
            <option value="warning">warning</option>
            <option value="critical">critical</option>
          </select>
          <span className="text-muted">, repeating</span>
          <select
            value={form.cooldown_s}
            onChange={(e) => setForm({ ...form, cooldown_s: Number(e.target.value) })}
            className="rounded-md border border-border bg-surface px-2 py-1.5"
          >
            {COOLDOWNS.map((c) => (
              <option key={c.value} value={c.value}>
                {c.label}
              </option>
            ))}
          </select>
          <button
            onClick={() => create.mutate()}
            disabled={!form.name.trim() || create.isPending}
            className="rounded-md bg-accent px-3 py-1.5 font-medium text-white disabled:opacity-40"
          >
            Add
          </button>
        </div>
        {error && <p className="text-sm text-danger">{error}</p>}
      </section>

      <section className="space-y-2">
        <h2 className="text-sm font-medium">Rules</h2>
        <div className="overflow-hidden rounded-lg border border-border">
          <table className="w-full text-sm">
            <thead className="bg-surface-raised text-left text-xs uppercase tracking-wide text-muted">
              <tr>
                <th className="px-4 py-2 font-medium">Rule</th>
                <th className="px-4 py-2 font-medium">Condition</th>
                <th className="px-4 py-2 font-medium">Firing</th>
                <th className="px-4 py-2 font-medium">Enabled</th>
                <th className="px-4 py-2" />
              </tr>
            </thead>
            <tbody>
              {(rules.data?.data.length ?? 0) === 0 && (
                <tr>
                  <td colSpan={5} className="px-4 py-6 text-center text-muted">
                    No alert rules yet.
                  </td>
                </tr>
              )}
              {rules.data?.data.map((rule) => (
                <tr key={rule.id} className="border-t border-border">
                  <td className="px-4 py-2">
                    {rule.name}
                    <span className="block text-xs text-muted">{rule.severity}</span>
                  </td>
                  <td className="px-4 py-2 text-muted">
                    {METRICS.find((m) => m.value === rule.metric)?.label ?? rule.metric}{' '}
                    {rule.op === '<' ? 'below' : 'above'} {formatValue(rule.metric, rule.threshold)}
                    {rule.duration_s > 0 && ` for ${Math.round(rule.duration_s / 60)} min`}
                  </td>
                  <td className="px-4 py-2">
                    {rule.firing_count > 0 ? (
                      <span className="text-danger">{rule.firing_count} VM(s)</span>
                    ) : (
                      <span className="text-muted">—</span>
                    )}
                  </td>
                  <td className="px-4 py-2">
                    <button
                      onClick={() => toggle.mutate(rule)}
                      className={`rounded-full px-2 py-0.5 text-xs ${
                        rule.is_enabled
                          ? 'bg-running/15 text-running'
                          : 'bg-stopped/15 text-stopped'
                      }`}
                    >
                      {rule.is_enabled ? 'enabled' : 'disabled'}
                    </button>
                  </td>
                  <td className="px-4 py-2 text-right">
                    <button
                      onClick={() => remove.mutate(rule.id)}
                      className="text-xs text-danger hover:underline"
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  )
}

// Mirrors the server's own formatting, so a threshold reads the same in the UI
// as it does in the message the alert sends.
function formatValue(metric: string, value: number): string {
  if (metric.endsWith('_pct')) return `${value.toFixed(1)}%`
  const units = ['B/s', 'KiB/s', 'MiB/s', 'GiB/s']
  let scaled = value
  let index = 0
  while (scaled >= 1024 && index < units.length - 1) {
    scaled /= 1024
    index++
  }
  return `${index === 0 ? Math.round(scaled) : scaled.toFixed(1)} ${units[index]}`
}
