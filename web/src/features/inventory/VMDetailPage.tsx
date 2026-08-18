import { lazy, Suspense, useEffect, useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { api, ApiError } from '@/api/client'
import { PowerControls, type PowerAction } from './PowerControls'
import type { HistoryEntry, VMDetail, VMState } from '@/api/types'
import { StateBadge } from '@/components/StateBadge'
import { absoluteTime, bytes, percent, relativeTime, uptime } from '@/lib/format'
import { useAuth } from '@/features/auth/useAuth'
import { can } from '@/lib/permissions'
import { RANGE_LABELS, RANGES, type Range } from './metrics'

// Charts are the heaviest thing the portal loads, and most visits to a VM
// never open the performance tab, so they arrive on demand (NFR-P5).
const PerformanceTab = lazy(() =>
  import('./PerformanceTab').then((m) => ({ default: m.PerformanceTab })),
)

type Tab = 'overview' | 'performance' | 'history'

const PENDING_LABEL: Record<PowerAction, string> = {
  start: 'Starting',
  stop: 'Stopping',
  shutdown: 'Shutting down',
  reboot: 'Rebooting',
}

export function VMDetailPage() {
  const { vmId = '' } = useParams()
  const [params, setParams] = useSearchParams()
  const tab = (params.get('tab') as Tab) || 'overview'
  const range = (params.get('range') as Range) || '24h'
  const { user } = useAuth()

  // A power action returns 202: the platform took the task, the machine has
  // not changed state yet. Until it does, the page polls faster than its
  // resting rate so the new state appears without a manual refresh — and gives
  // up after a while, because an action the platform accepted and then failed
  // to carry out must not leave the page polling for the rest of the session.
  const [pending, setPending] = useState<{ action: PowerAction; from: VMState } | null>(null)

  const vm = useQuery({
    queryKey: ['vm', vmId],
    queryFn: () => api.get<VMDetail>(`/vms/${vmId}`),
    refetchInterval: pending ? 2_000 : 30_000,
  })

  const observed = vm.data?.state
  useEffect(() => {
    if (!pending) return
    if (observed && observed !== pending.from) {
      setPending(null)
      return
    }
    const giveUp = window.setTimeout(() => setPending(null), 90_000)
    return () => window.clearTimeout(giveUp)
  }, [pending, observed])

  if (vm.isLoading) return <p className="text-sm text-muted">Loading…</p>

  if (vm.error) {
    const missing = vm.error instanceof ApiError && vm.error.status === 404
    return (
      <div className="space-y-3">
        <p className="text-sm text-danger">
          {missing
            ? 'This virtual machine does not exist, or is not visible to your account.'
            : 'Could not load this virtual machine.'}
        </p>
        <Link to="/vms" className="text-sm text-accent hover:underline">
          Back to inventory
        </Link>
      </div>
    )
  }
  if (!vm.data) return null

  const detail = vm.data

  function selectTab(next: Tab) {
    const updated = new URLSearchParams(params)
    updated.set('tab', next)
    setParams(updated, { replace: true })
  }

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="space-y-1">
          <Link to="/vms" className="text-sm text-muted hover:text-accent">
            ← Inventory
          </Link>
          <div className="flex items-center gap-3">
            <h1 className="text-xl font-semibold">{detail.name}</h1>
            <StateBadge
              state={detail.state}
              stale={detail.sync_state === 'missing'}
              liveAt={detail.live_at}
            />
          </div>
          <p className="text-sm text-muted">
            {detail.vm_type} · {detail.external_id} · {detail.platform_name}
            {detail.host_name && ` · ${detail.host_name}`}
          </p>
        </div>

        <div className="flex flex-col items-end gap-2">
          <div className="flex flex-wrap items-center justify-end gap-2">
            {user && can.openConsole(user.role) && (
              <a
                href={`/console/${detail.id}`}
                target="_blank"
                rel="noreferrer"
                // A console opens in its own tab so that navigating the portal
                // does not tear down a session someone is working in.
                className={`rounded-md px-3 py-2 text-sm font-medium ${
                  detail.state === 'running'
                    ? 'bg-accent text-white'
                    : 'pointer-events-none border border-border text-muted opacity-60'
                }`}
                title={detail.state === 'running' ? undefined : 'The VM is not running'}
              >
                Open console
              </a>
            )}

            {user && can.openSsh(user.role) && (
              <a
                href={`/ssh/${detail.id}`}
                target="_blank"
                rel="noreferrer"
                // Its own tab, for the same reason as the console: a live
                // session must survive navigating the portal (SSH-01).
                className={`rounded-md border px-3 py-2 text-sm font-medium ${
                  detail.state === 'running' && detail.ip_addresses.length > 0
                    ? 'border-border hover:bg-surface-raised'
                    : 'pointer-events-none border-border text-muted opacity-60'
                }`}
                title={
                  detail.state !== 'running'
                    ? 'The VM is not running'
                    : detail.ip_addresses.length === 0
                      ? 'No address is known for this VM yet — it needs the guest agent'
                      : undefined
                }
              >
                SSH
              </a>
            )}

            {user && can.powerActions(user.role) && detail.sync_state !== 'missing' && (
              <PowerControls
                vmId={detail.id}
                state={detail.state}
                onRequested={(action) => setPending({ action, from: detail.state })}
              />
            )}
          </div>

          {pending && (
            <p aria-live="polite" className="text-xs text-muted">
              {PENDING_LABEL[pending.action]} — waiting for the platform.
            </p>
          )}
        </div>
      </div>

      <nav className="flex gap-1 border-b border-border">
        {(['overview', 'performance', 'history'] as Tab[]).map((name) => (
          <button
            key={name}
            onClick={() => selectTab(name)}
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

      {tab === 'overview' && <Overview vm={detail} />}

      {tab === 'performance' && (
        <Suspense fallback={<p className="text-sm text-muted">Loading charts…</p>}>
          <PerformanceTab
            vmId={detail.id}
            range={range}
            onRangeChange={(next) => {
              const updated = new URLSearchParams(params)
              updated.set('range', next)
              setParams(updated, { replace: true })
            }}
          />
        </Suspense>
      )}

      {tab === 'history' && <History vmId={detail.id} />}
    </div>
  )
}

function Overview({ vm }: { vm: VMDetail }) {
  const agentUnavailable = vm.attrs?.['guest_agent'] === 'unavailable'

  return (
    <div className="grid gap-5 lg:grid-cols-2">
      <section className="space-y-3 rounded-lg border border-border bg-surface-raised p-4">
        <h2 className="text-sm font-medium text-muted">Identity</h2>
        <dl className="grid grid-cols-2 gap-y-2 text-sm">
          <Field label="Platform" value={vm.platform_name} />
          <Field label="Datacenter" value={vm.datacenter} />
          <Field label="Node" value={vm.host_name || '—'} />
          <Field label="VMID" value={vm.external_id} />
          <Field label="Type" value={vm.vm_type} />
          <Field label="Uptime" value={vm.state === 'running' ? uptime(vm.uptime_s) : '—'} />
          <Field
            label="First seen"
            value={relativeTime(vm.first_seen_at)}
            title={absoluteTime(vm.first_seen_at)}
          />
          <Field
            label="Last synced"
            value={relativeTime(vm.last_seen_at)}
            title={absoluteTime(vm.last_seen_at)}
          />
        </dl>
      </section>

      <section className="space-y-3 rounded-lg border border-border bg-surface-raised p-4">
        <h2 className="text-sm font-medium text-muted">Resources</h2>
        <dl className="grid grid-cols-2 gap-y-2 text-sm">
          <Field label="vCPU" value={String(vm.cpu_cores || '—')} />
          <Field label="Memory" value={bytes(vm.memory_bytes)} />
          <Field label="Disk" value={bytes(vm.disk_bytes)} />
          <Field label="Current CPU" value={vm.state === 'running' ? percent(vm.cpu_pct) : '—'} />
        </dl>

        <div className="space-y-1 pt-2">
          <div className="text-xs uppercase tracking-wide text-muted">Addresses</div>
          {vm.ip_addresses.length > 0 ? (
            <ul className="space-y-0.5 text-sm">
              {vm.ip_addresses.map((ip) => (
                <li key={ip} className="font-mono text-xs">
                  {ip}
                </li>
              ))}
            </ul>
          ) : (
            // Explaining the gap beats an empty field: without this an
            // operator cannot tell a broken portal from an unconfigured agent.
            <p className="text-sm text-muted">
              {agentUnavailable
                ? 'None reported — the guest agent is not configured on this VM.'
                : 'None reported.'}
            </p>
          )}
        </div>
      </section>

      <section className="space-y-3 rounded-lg border border-border bg-surface-raised p-4 lg:col-span-2">
        <h2 className="text-sm font-medium text-muted">Grouping</h2>
        <div className="flex flex-wrap gap-4 text-sm">
          <TagRow label="Portal tags" tags={vm.portal_tags} empty="none set" />
          <TagRow label="Platform tags" tags={vm.platform_tags} empty="none on the platform" />
          <TagRow label="VM groups" tags={vm.groups} empty="not in a group" />
        </div>
        {vm.notes && (
          <div className="space-y-1 border-t border-border pt-3">
            <div className="text-xs uppercase tracking-wide text-muted">Notes</div>
            <p className="whitespace-pre-wrap text-sm">{vm.notes}</p>
          </div>
        )}
      </section>
    </div>
  )
}

function Field({ label, value, title }: { label: string; value: string; title?: string }) {
  return (
    <>
      <dt className="text-muted">{label}</dt>
      <dd title={title}>{value}</dd>
    </>
  )
}

function TagRow({ label, tags, empty }: { label: string; tags: string[]; empty: string }) {
  return (
    <div className="space-y-1">
      <div className="text-xs uppercase tracking-wide text-muted">{label}</div>
      {tags.length === 0 ? (
        <span className="text-sm text-muted">{empty}</span>
      ) : (
        <div className="flex flex-wrap gap-1">
          {tags.map((tag) => (
            <span key={tag} className="rounded-full bg-accent/10 px-2 py-0.5 text-xs text-accent">
              {tag}
            </span>
          ))}
        </div>
      )}
    </div>
  )
}

function History({ vmId }: { vmId: string }) {
  const [limit] = useState(100)
  const history = useQuery({
    queryKey: ['vm-history', vmId, limit],
    queryFn: () => api.get<{ data: HistoryEntry[] }>(`/vms/${vmId}/history?limit=${limit}`),
  })

  if (history.isLoading) return <p className="text-sm text-muted">Loading…</p>
  if (history.error) return <p className="text-sm text-danger">Could not load history.</p>

  const entries = history.data?.data ?? []
  if (entries.length === 0) return <p className="text-sm text-muted">No recorded changes.</p>

  return (
    <div className="overflow-hidden rounded-lg border border-border">
      <table className="w-full text-sm">
        <thead className="bg-surface-raised text-left text-xs uppercase tracking-wide text-muted">
          <tr>
            <th className="px-4 py-2 font-medium">When</th>
            <th className="px-4 py-2 font-medium">Field</th>
            <th className="px-4 py-2 font-medium">Change</th>
          </tr>
        </thead>
        <tbody>
          {entries.map((entry, index) => (
            <tr key={`${entry.changed_at}-${index}`} className="border-t border-border">
              <td className="px-4 py-2 text-muted" title={absoluteTime(entry.changed_at)}>
                {relativeTime(entry.changed_at)}
              </td>
              <td className="px-4 py-2 font-mono text-xs">{describeField(entry.field)}</td>
              <td className="px-4 py-2">
                {entry.field.startsWith('_') ? (
                  <span className="text-muted">{entry.old_value || entry.new_value || '—'}</span>
                ) : (
                  <span>
                    <span className="text-muted line-through">{entry.old_value || '—'}</span>
                    {' → '}
                    <span>{entry.new_value || '—'}</span>
                  </span>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  )
}

// The reconciler records lifecycle events under underscore-prefixed
// pseudo-fields; translate them rather than showing the raw key.
function describeField(field: string): string {
  return { _created: 'discovered', _deleted: 'deleted', _missing: 'went missing' }[field] ?? field
}

export { RANGES, RANGE_LABELS }
