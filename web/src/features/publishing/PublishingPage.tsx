import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, ApiError } from '@/api/client'
import type { EdgeProvider, IngressRule, IngressView, PublishedApp } from '@/api/types'
import { relativeTime } from '@/lib/format'
import { PublishDialog } from './PublishDialog'
import { ProviderSetup } from './ProviderSetup'

export function PublishingPage() {
  const [publishing, setPublishing] = useState(false)
  const [error, setError] = useState('')
  const queryClient = useQueryClient()

  const providers = useQuery({
    queryKey: ['edge-providers'],
    queryFn: () => api.get<{ data: EdgeProvider[] }>('/edge-providers'),
    refetchInterval: 60_000,
  })

  const provider = providers.data?.data?.[0]

  // The live routing table, always from the provider. The portal's copy of
  // what it published is not the truth; what the tunnel is serving is.
  const ingress = useQuery({
    queryKey: ['edge-ingress', provider?.id],
    queryFn: () => api.get<IngressView>(`/edge-providers/${provider!.id}/ingress`),
    enabled: Boolean(provider?.ready),
    refetchInterval: 60_000,
  })

  const apps = useQuery({
    queryKey: ['published-apps', provider?.id],
    queryFn: () => api.get<{ data: PublishedApp[] }>(`/edge-providers/${provider!.id}/apps`),
    enabled: Boolean(provider?.ready),
  })

  const unpublish = useMutation({
    mutationFn: (app: PublishedApp) =>
      api.del(`/published-apps/${app.id}?read_version=${ingress.data?.version ?? 0}`),
    onSuccess: () => {
      setError('')
      void queryClient.invalidateQueries({ queryKey: ['edge-ingress'] })
      void queryClient.invalidateQueries({ queryKey: ['published-apps'] })
    },
    onError: (err) =>
      setError(err instanceof ApiError ? err.detail : 'The app could not be unpublished.'),
  })

  if (providers.isLoading) return <p className="text-sm text-muted">Loading…</p>

  if (!provider) {
    return (
      <ProviderSetup
        onDone={() => void queryClient.invalidateQueries({ queryKey: ['edge-providers'] })}
      />
    )
  }

  const appByRoute = new Map((apps.data?.data ?? []).map((a) => [a.hostname + (a.path ?? ''), a]))

  return (
    <div className="space-y-5">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="text-xl font-semibold">Published apps</h1>
          <p className="text-sm text-muted">
            Services reachable from the internet through{' '}
            <span className="font-medium">{provider.tunnel_name || provider.name}</span>. Everything
            below is read from Cloudflare.
          </p>
        </div>
        <button
          onClick={() => setPublishing(true)}
          disabled={!provider.ready}
          className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
        >
          Publish an app
        </button>
      </div>

      <ProviderHealth provider={provider} />

      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}

      {ingress.isLoading && <p className="text-sm text-muted">Reading the routing table…</p>}
      {ingress.error && (
        <p className="text-sm text-danger">
          {ingress.error instanceof ApiError
            ? ingress.error.detail
            : 'Could not read the routing table.'}
        </p>
      )}

      {ingress.data && (
        <>
          <RuleTable
            view={ingress.data}
            appFor={(rule) => appByRoute.get(rule.hostname + (rule.path ?? ''))}
            onUnpublish={(app) => unpublish.mutate(app)}
            busy={unpublish.isPending}
          />
          <p className="text-xs text-muted">
            {ingress.data.rules.length} rules · {ingress.data.portal_owned} published here ·{' '}
            {ingress.data.external} already existed
            {ingress.data.unmatched > 0 &&
              ` · ${ingress.data.unmatched} pointing at addresses no VM holds`}
          </p>
        </>
      )}

      {publishing && provider.ready && (
        <PublishDialog
          providerID={provider.id}
          version={ingress.data?.version ?? 0}
          onClose={() => setPublishing(false)}
          onPublished={() => {
            setPublishing(false)
            void queryClient.invalidateQueries({ queryKey: ['edge-ingress'] })
            void queryClient.invalidateQueries({ queryKey: ['published-apps'] })
          }}
        />
      )}
    </div>
  )
}

function ProviderHealth({ provider }: { provider: EdgeProvider }) {
  const tone =
    provider.health === 'healthy'
      ? 'border-running/40 bg-running/5'
      : provider.health === 'degraded'
        ? 'border-warning/40 bg-warning/5'
        : provider.health === 'unreachable'
          ? 'border-danger/40 bg-danger/5'
          : 'border-border'

  return (
    <div className={`rounded-md border px-3 py-2 text-sm ${tone}`}>
      <span className="font-medium capitalize">{provider.health}</span>
      {provider.health_detail && <> — {provider.health_detail}</>}
      {provider.last_seen_at && (
        <span className="text-muted"> · last reached {relativeTime(provider.last_seen_at)}</span>
      )}
      {!provider.ready && (
        <span className="text-muted"> · no tunnel selected, so nothing can be published yet</span>
      )}
    </div>
  )
}

function RuleTable({
  view,
  appFor,
  onUnpublish,
  busy,
}: {
  view: IngressView
  appFor: (rule: IngressRule) => PublishedApp | undefined
  onUnpublish: (app: PublishedApp) => void
  busy: boolean
}) {
  return (
    <div className="overflow-x-auto rounded-md border border-border">
      <table className="tabular-nums w-full min-w-[48rem] text-sm">
        <thead className="bg-surface-inset text-left text-xs uppercase text-muted">
          <tr>
            <th className="px-3 py-2 font-medium">Hostname</th>
            <th className="px-3 py-2 font-medium">Target</th>
            <th className="px-3 py-2 font-medium">Machine</th>
            <th className="px-3 py-2 font-medium">Origin</th>
            <th className="px-3 py-2" />
          </tr>
        </thead>
        <tbody>
          {view.rules.map((rule) => {
            const app = appFor(rule)
            return (
              <tr key={rule.index} className="border-t border-border">
                <td className="px-3 py-2">
                  {rule.is_catch_all ? (
                    <span className="text-muted">anything else</span>
                  ) : (
                    <a
                      href={`https://${rule.hostname}${rule.path ?? ''}`}
                      target="_blank"
                      rel="noreferrer"
                      className="text-accent hover:underline"
                    >
                      {rule.hostname}
                      {rule.path}
                    </a>
                  )}
                  {rule.is_portal && (
                    <span className="ml-2 rounded bg-accent/10 px-1.5 py-0.5 text-xs text-accent">
                      this portal
                    </span>
                  )}
                </td>
                <td className="px-3 py-2 font-mono text-xs">{rule.service}</td>
                <td className="px-3 py-2">
                  {rule.vm ? (
                    <span>
                      {rule.vm.name} <span className="text-xs text-muted">({rule.vm.state})</span>
                    </span>
                  ) : rule.unmatched ? (
                    // The drift this panel exists to surface: a VM that moved
                    // or was deleted leaves a rule pointing at nothing.
                    <span className="text-xs text-warning">no VM has this address</span>
                  ) : (
                    <span className="text-xs text-muted">—</span>
                  )}
                </td>
                <td className="px-3 py-2">
                  <OriginBadge rule={rule} />
                </td>
                <td className="px-3 py-2 text-right">
                  {app ? (
                    <button
                      onClick={() => onUnpublish(app)}
                      disabled={busy}
                      className="rounded-md border border-border px-2 py-1 text-xs hover:bg-surface-inset disabled:opacity-40"
                      title={
                        app.manages_dns
                          ? 'Removes the routing rule and the DNS record the portal created'
                          : 'Removes the routing rule. The DNS record was not created here, so it is left alone'
                      }
                    >
                      Unpublish
                    </button>
                  ) : null}
                </td>
              </tr>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

function OriginBadge({ rule }: { rule: IngressRule }) {
  if (rule.is_catch_all) {
    return <span className="text-xs text-muted">terminator</span>
  }
  if (rule.origin === 'portal') {
    return <span className="text-xs text-accent">published here</span>
  }
  // Rules the portal did not create are read-only and preserved exactly. Said
  // plainly so nobody wonders why there is no Unpublish button.
  return (
    <span className="text-xs text-muted" title="Created outside the portal; shown read-only">
      already existed
    </span>
  )
}
