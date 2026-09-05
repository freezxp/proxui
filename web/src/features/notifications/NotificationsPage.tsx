import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, ApiError } from '@/api/client'
import type {
  Delivery,
  NotificationChannel,
  NotificationRule,
  Platform,
  VMGroup,
} from '@/api/types'
import { Drawer } from '@/components/Drawer'
import { absoluteTime, relativeTime } from '@/lib/format'
import { AlertsPanel } from './AlertsPanel'

type Tab = 'channels' | 'routing' | 'alerts' | 'deliveries'

const CATEGORIES = [
  { value: 'sync_failure', label: 'Synchronization failures' },
  { value: 'vm_state_change', label: 'VM state changes' },
  { value: 'performance_alert', label: 'Performance alerts' },
  { value: 'security', label: 'Security events' },
]

const SEVERITIES = ['info', 'warning', 'critical']

export function NotificationsPage() {
  const [tab, setTab] = useState<Tab>('channels')

  return (
    <div className="space-y-5">
      <div>
        <h1 className="text-xl font-semibold">Notifications</h1>
        <p className="text-sm text-muted">
          Where the portal sends what it notices, and whether it arrived.
        </p>
      </div>

      <nav className="flex gap-1 border-b border-border">
        {(['channels', 'routing', 'alerts', 'deliveries'] as Tab[]).map((name) => (
          <button
            key={name}
            onClick={() => setTab(name)}
            className={`-mb-px border-b-2 px-4 py-2 text-sm capitalize ${
              tab === name
                ? 'border-accent font-medium text-accent'
                : 'border-transparent text-muted hover:text-content'
            }`}
          >
            {name}
          </button>
        ))}
      </nav>

      {tab === 'channels' && <ChannelsTab />}
      {tab === 'routing' && <RoutingTab />}
      {tab === 'alerts' && <AlertsPanel />}
      {tab === 'deliveries' && <DeliveriesTab />}
    </div>
  )
}

function ChannelsTab() {
  const queryClient = useQueryClient()
  const [editing, setEditing] = useState<NotificationChannel | 'new' | null>(null)
  const [tested, setTested] = useState<Record<string, string>>({})

  const channels = useQuery({
    queryKey: ['channels'],
    queryFn: () => api.get<{ data: NotificationChannel[] }>('/notification-channels'),
  })

  const test = useMutation({
    mutationFn: (id: string) =>
      api.post<{ delivered: boolean }>(`/notification-channels/${id}/test`, {}),
    onSuccess: (_, id) => setTested((prev) => ({ ...prev, [id]: 'Delivered.' })),
    onError: (err, id) => {
      // The reason is the whole point of a test: "failed" alone leaves an
      // administrator with nothing to change.
      const detail =
        err instanceof ApiError && err.body && typeof err.body === 'object'
          ? ((err.body as { error?: string }).error ?? err.message)
          : 'The test could not run.'
      setTested((prev) => ({ ...prev, [id]: detail }))
    },
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/notification-channels/${id}`),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['channels'] }),
  })

  if (channels.isLoading) return <p className="text-sm text-muted">Loading…</p>

  const rows = channels.data?.data ?? []

  return (
    <div className="space-y-4">
      <div className="flex justify-end">
        <button
          onClick={() => setEditing('new')}
          className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white"
        >
          Add channel
        </button>
      </div>

      {rows.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border p-8 text-center text-sm text-muted">
          No channels yet. Without one, nothing the portal notices leaves it.
        </div>
      ) : (
        <div className="grid gap-3 md:grid-cols-2">
          {rows.map((channel) => (
            <div key={channel.id} className="space-y-2 rounded-lg border border-border p-4">
              <div className="flex items-start justify-between">
                <div>
                  <div className="font-medium">{channel.name}</div>
                  <div className="text-xs text-muted">
                    {channel.kind}
                    {typeof channel.config.url === 'string' && ` · ${channel.config.url}`}
                    {typeof channel.config.host === 'string' && ` · ${channel.config.host}`}
                  </div>
                </div>
                <span
                  className={`rounded-full px-2 py-0.5 text-xs ${
                    channel.is_enabled ? 'bg-running/15 text-running' : 'bg-stopped/15 text-stopped'
                  }`}
                >
                  {channel.is_enabled ? 'enabled' : 'disabled'}
                </span>
              </div>

              {!channel.has_secret && channel.kind !== 'webhook' && (
                <p className="text-xs text-paused">
                  No secret stored. This channel will fail until one is set.
                </p>
              )}
              {tested[channel.id] && <p className="text-xs text-muted">{tested[channel.id]}</p>}

              <div className="flex gap-3 text-xs">
                <button
                  onClick={() => test.mutate(channel.id)}
                  disabled={test.isPending}
                  className="text-accent hover:underline"
                >
                  Send test
                </button>
                <button onClick={() => setEditing(channel)} className="text-accent hover:underline">
                  Edit
                </button>
                <button
                  onClick={() => remove.mutate(channel.id)}
                  className="text-danger hover:underline"
                >
                  Delete
                </button>
              </div>
            </div>
          ))}
        </div>
      )}

      {editing && (
        <ChannelForm
          channel={editing === 'new' ? null : editing}
          onClose={() => setEditing(null)}
          onSaved={() => {
            setEditing(null)
            void queryClient.invalidateQueries({ queryKey: ['channels'] })
          }}
        />
      )}
    </div>
  )
}

// Each channel kind needs different fields. They are declared here rather than
// by the server because, unlike a connector, the set of delivery mechanisms is
// fixed by what the portal itself implements.
const CHANNEL_FIELDS: Record<string, { key: string; label: string; help?: string }[]> = {
  email: [
    { key: 'host', label: 'SMTP host' },
    { key: 'port', label: 'Port', help: 'Defaults to 587.' },
    { key: 'from', label: 'From address' },
    { key: 'to', label: 'Recipients', help: 'Comma separated.' },
    { key: 'username', label: 'Username', help: 'Leave empty for an unauthenticated relay.' },
  ],
  slack: [],
  webhook: [{ key: 'url', label: 'URL' }],
}

const SECRET_LABEL: Record<string, string> = {
  email: 'SMTP password',
  slack: 'Incoming webhook URL',
  webhook: 'HMAC signing key',
}

const SECRET_HELP: Record<string, string> = {
  email: 'Stored encrypted.',
  slack: 'The URL is the credential: anyone holding it can post to that channel.',
  webhook: 'Optional. When set, deliveries carry an HMAC-SHA256 signature the receiver can verify.',
}

function ChannelForm({
  channel,
  onClose,
  onSaved,
}: {
  channel: NotificationChannel | null
  onClose: () => void
  onSaved: () => void
}) {
  const editing = channel !== null
  const [kind, setKind] = useState(channel?.kind ?? 'slack')
  const [name, setName] = useState(channel?.name ?? '')
  const [config, setConfig] = useState<Record<string, string>>(() => {
    const out: Record<string, string> = {}
    for (const [key, value] of Object.entries(channel?.config ?? {})) {
      out[key] = Array.isArray(value) ? value.join(', ') : String(value ?? '')
    }
    return out
  })
  const [secret, setSecret] = useState('')
  const [error, setError] = useState('')

  const save = useMutation({
    mutationFn: () => {
      const body = { name, kind, config: buildConfig(kind, config), secret }
      return editing
        ? api.put(`/notification-channels/${channel.id}`, body)
        : api.post('/notification-channels', body)
    },
    onSuccess: onSaved,
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not save the channel.'),
  })

  const fields = CHANNEL_FIELDS[kind] ?? []
  const missing =
    !name.trim() ||
    fields.some((f) => f.key !== 'username' && f.key !== 'port' && !config[f.key]?.trim())

  return (
    <Drawer
      title={editing ? `Edit ${channel.name}` : 'Add channel'}
      onClose={onClose}
      footer={
        <div className="flex justify-end gap-2">
          <button onClick={onClose} className="rounded-md border border-border px-3 py-2 text-sm">
            Cancel
          </button>
          <button
            onClick={() => save.mutate()}
            disabled={missing || save.isPending}
            className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
          >
            {editing ? 'Save changes' : 'Add channel'}
          </button>
        </div>
      }
    >
      <div className="space-y-4">
        <Field label="Type">
          <select
            value={kind}
            disabled={editing}
            onChange={(e) => setKind(e.target.value as NotificationChannel['kind'])}
            className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm disabled:opacity-60"
          >
            <option value="slack">Slack</option>
            <option value="email">Email (SMTP)</option>
            <option value="webhook">Webhook</option>
          </select>
        </Field>

        <Field label="Name">
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
          />
        </Field>

        {fields.map((field) => (
          <Field key={field.key} label={field.label} help={field.help}>
            <input
              value={config[field.key] ?? ''}
              onChange={(e) => setConfig((prev) => ({ ...prev, [field.key]: e.target.value }))}
              className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
            />
          </Field>
        ))}

        <Field label={SECRET_LABEL[kind]} help={SECRET_HELP[kind]}>
          <input
            type="password"
            value={secret}
            autoComplete="new-password"
            placeholder={editing && channel.has_secret ? 'unchanged' : ''}
            onChange={(e) => setSecret(e.target.value)}
            className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
          />
          {editing && channel.has_secret && (
            <p className="text-xs text-muted">Leave empty to keep the stored secret.</p>
          )}
        </Field>

        {error && <p className="text-sm text-danger">{error}</p>}
      </div>
    </Drawer>
  )
}

// Recipients are typed as one line but stored as a list, so the split happens
// here rather than leaving the server to guess.
function buildConfig(kind: string, raw: Record<string, string>): Record<string, unknown> {
  const out: Record<string, unknown> = {}
  for (const field of CHANNEL_FIELDS[kind] ?? []) {
    const value = raw[field.key]?.trim()
    if (!value) continue
    out[field.key] = field.key === 'to' ? value.split(',').map((s) => s.trim()) : value
  }
  return out
}

function RoutingTab() {
  const queryClient = useQueryClient()
  const [category, setCategory] = useState(CATEGORIES[0].value)
  const [severity, setSeverity] = useState('info')
  const [channelID, setChannelID] = useState('')
  const [platformID, setPlatformID] = useState('')
  const [vmGroupID, setVMGroupID] = useState('')
  const [error, setError] = useState('')

  const rules = useQuery({
    queryKey: ['notification-rules'],
    queryFn: () => api.get<{ data: NotificationRule[] }>('/notification-rules'),
  })
  const channels = useQuery({
    queryKey: ['channels'],
    queryFn: () => api.get<{ data: NotificationChannel[] }>('/notification-channels'),
  })
  const platforms = useQuery({
    queryKey: ['platforms'],
    queryFn: () => api.get<{ data: Platform[] }>('/platforms'),
  })
  const groups = useQuery({
    queryKey: ['vm-groups'],
    queryFn: () => api.get<{ data: VMGroup[] }>('/vm-groups'),
  })

  const create = useMutation({
    mutationFn: () =>
      api.post('/notification-rules', {
        category,
        min_severity: severity,
        channel_id: channelID,
        platform_id: platformID || null,
        vm_group_id: vmGroupID || null,
      }),
    onSuccess: () => {
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['notification-rules'] })
    },
    onError: (err) => setError(err instanceof Error ? err.message : 'Could not add the rule.'),
  })

  const remove = useMutation({
    mutationFn: (id: string) => api.del(`/notification-rules/${id}`),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['notification-rules'] }),
  })

  const availableChannels = channels.data?.data ?? []

  return (
    <div className="space-y-4">
      {availableChannels.length === 0 ? (
        <div className="rounded-lg border border-dashed border-border p-8 text-center text-sm text-muted">
          Add a channel first; a rule needs somewhere to deliver to.
        </div>
      ) : (
        <div className="grid gap-3 rounded-lg border border-border p-3 lg:grid-cols-6">
          <Labelled label="When">
            <select
              value={category}
              onChange={(e) => setCategory(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm"
            >
              {CATEGORIES.map((c) => (
                <option key={c.value} value={c.value}>
                  {c.label}
                </option>
              ))}
            </select>
          </Labelled>
          <Labelled label="At least">
            <select
              value={severity}
              onChange={(e) => setSeverity(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm"
            >
              {SEVERITIES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </Labelled>
          <Labelled label="Platform">
            <select
              value={platformID}
              onChange={(e) => setPlatformID(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm"
            >
              <option value="">Any</option>
              {platforms.data?.data.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.name}
                </option>
              ))}
            </select>
          </Labelled>
          <Labelled label="VM group">
            <select
              value={vmGroupID}
              onChange={(e) => setVMGroupID(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm"
            >
              <option value="">Any</option>
              {groups.data?.data.map((g) => (
                <option key={g.id} value={g.id}>
                  {g.name}
                </option>
              ))}
            </select>
          </Labelled>
          <Labelled label="Send to">
            <select
              value={channelID}
              onChange={(e) => setChannelID(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 text-sm"
            >
              <option value="">Pick a channel</option>
              {availableChannels.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </Labelled>
          <div className="flex items-end">
            <button
              onClick={() => create.mutate()}
              disabled={!channelID || create.isPending}
              className="w-full rounded-md border border-border px-3 py-1.5 text-sm disabled:opacity-40"
            >
              Add rule
            </button>
          </div>
        </div>
      )}

      {error && <p className="text-sm text-danger">{error}</p>}

      <div className="overflow-hidden rounded-lg border border-border">
        <table className="tabular-nums w-full text-sm">
          <thead className="bg-surface-inset text-left text-xs uppercase tracking-wide text-muted">
            <tr>
              <th className="px-4 py-2 font-medium">When</th>
              <th className="px-4 py-2 font-medium">At least</th>
              <th className="px-4 py-2 font-medium">Scope</th>
              <th className="px-4 py-2 font-medium">Send to</th>
              <th className="px-4 py-2" />
            </tr>
          </thead>
          <tbody>
            {(rules.data?.data.length ?? 0) === 0 && (
              <tr>
                <td colSpan={5} className="px-4 py-6 text-center text-muted">
                  No routing rules. Events are recorded but nothing is sent.
                </td>
              </tr>
            )}
            {rules.data?.data.map((rule) => (
              <tr key={rule.id} className="border-t border-border">
                <td className="px-4 py-2">
                  {CATEGORIES.find((c) => c.value === rule.category)?.label ?? rule.category}
                </td>
                <td className="px-4 py-2">{rule.min_severity}</td>
                <td className="px-4 py-2 text-muted">
                  {rule.platform_id || rule.vm_group_id ? 'narrowed' : 'everything'}
                </td>
                <td className="px-4 py-2">{rule.channel_name}</td>
                <td className="px-4 py-2 text-right">
                  <button
                    onClick={() => remove.mutate(rule.id)}
                    className="text-xs text-danger hover:underline"
                  >
                    Remove
                  </button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}

function DeliveriesTab() {
  const deliveries = useQuery({
    queryKey: ['deliveries'],
    queryFn: () => api.get<{ data: Delivery[] }>('/notification-deliveries?limit=200'),
    refetchInterval: 20_000,
  })

  if (deliveries.isLoading) return <p className="text-sm text-muted">Loading…</p>

  const rows = deliveries.data?.data ?? []

  return (
    <div className="overflow-hidden rounded-lg border border-border">
      <table className="tabular-nums w-full text-sm">
        <thead className="bg-surface-inset text-left text-xs uppercase tracking-wide text-muted">
          <tr>
            <th className="px-4 py-2 font-medium">When</th>
            <th className="px-4 py-2 font-medium">Channel</th>
            <th className="px-4 py-2 font-medium">Subject</th>
            <th className="px-4 py-2 font-medium">Result</th>
          </tr>
        </thead>
        <tbody>
          {rows.length === 0 && (
            <tr>
              <td colSpan={4} className="px-4 py-6 text-center text-muted">
                Nothing has been sent yet.
              </td>
            </tr>
          )}
          {rows.map((delivery) => (
            <tr key={delivery.id} className="border-t border-border align-top">
              <td className="px-4 py-2 text-muted" title={absoluteTime(delivery.created_at)}>
                {relativeTime(delivery.created_at)}
              </td>
              <td className="px-4 py-2">{delivery.channel_name}</td>
              <td className="px-4 py-2">{delivery.subject}</td>
              <td className="px-4 py-2">
                <span
                  className={
                    delivery.status === 'sent'
                      ? 'text-running'
                      : delivery.status === 'pending'
                        ? 'text-paused'
                        : 'text-danger'
                  }
                >
                  {delivery.status}
                </span>
                {delivery.attempts > 1 && (
                  <span className="text-muted"> after {delivery.attempts} attempts</span>
                )}
                {delivery.last_error && (
                  <span className="block text-xs text-danger">{delivery.last_error}</span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

function Field({
  label,
  help,
  children,
}: {
  label: string
  help?: string
  children: React.ReactNode
}) {
  return (
    <div className="space-y-1">
      <span className="block text-sm">{label}</span>
      {children}
      {help && <p className="text-xs text-muted">{help}</p>}
    </div>
  )
}

function Labelled({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="space-y-1 text-xs">
      <span className="block uppercase tracking-wide text-muted">{label}</span>
      {children}
    </label>
  )
}
