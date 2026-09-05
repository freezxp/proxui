import { useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { api, ApiError } from '@/api/client'
import type { EdgeHealth, EdgeProvider, EdgeTunnel } from '@/api/types'

/**
 * Registering a Cloudflare account, in the order the information becomes
 * available: the credential has to work before its tunnels can be listed, and
 * a tunnel has to be chosen before anything can be published.
 *
 * Testing before saving is not politeness. Cloudflare shows an API token once;
 * discovering after the fact that it was mistyped means going back for another.
 */
export function ProviderSetup({ onDone }: { onDone: () => void }) {
  const [name, setName] = useState('cloudflare')
  const [accountID, setAccountID] = useState('')
  const [token, setToken] = useState('')
  const [health, setHealth] = useState<EdgeHealth | null>(null)
  const [tunnelID, setTunnelID] = useState('')
  const [zoneIDs, setZoneIDs] = useState<string[]>([])
  const [error, setError] = useState('')

  const test = useMutation({
    mutationFn: () =>
      api.post<EdgeHealth>('/edge-providers/test', { account_id: accountID.trim(), token }),
    onSuccess: (result) => {
      setError('')
      setHealth(result)
    },
    onError: (err) => {
      setHealth(null)
      setError(err instanceof ApiError ? err.detail : 'The credential could not be checked.')
    },
  })

  const save = useMutation({
    mutationFn: () => {
      const tunnel = health?.tunnels.find((t) => t.id === tunnelID)
      return api.post<EdgeProvider>('/edge-providers', {
        name: name.trim(),
        account_id: accountID.trim(),
        token,
        tunnel_id: tunnelID,
        tunnel_name: tunnel?.name ?? '',
        allowed_zone_ids: zoneIDs,
      })
    },
    onSuccess: onDone,
    onError: (err) =>
      setError(err instanceof ApiError ? err.detail : 'The provider could not be saved.'),
  })

  const usable = health?.tunnels.filter((t) => t.manageable) ?? []
  const unusable = health?.tunnels.filter((t) => !t.manageable) ?? []

  return (
    <div className="max-w-2xl space-y-5">
      <div>
        <h1 className="text-xl font-semibold">Published apps</h1>
        <p className="text-sm text-muted">
          Connect a Cloudflare account to publish services through a tunnel. The API token is stored
          encrypted and is never shown again.
        </p>
      </div>

      <div className="space-y-3 rounded-md border border-border p-4">
        <Field label="Name" htmlFor="edge-name">
          <input
            id="edge-name"
            value={name}
            onChange={(e) => setName(e.target.value)}
            className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
          />
        </Field>

        <Field
          label="Account ID"
          htmlFor="edge-account"
          help="From the Cloudflare dashboard sidebar. The portal cannot discover it: a resource-scoped token reports no accounts at all."
        >
          <input
            id="edge-account"
            value={accountID}
            onChange={(e) => setAccountID(e.target.value)}
            className="w-full rounded-md border border-border bg-surface px-3 py-2 font-mono text-sm"
          />
        </Field>

        <Field
          label="API token"
          htmlFor="edge-token"
          help="Needs Cloudflare Tunnel: Edit on the account and DNS: Edit on the zones you will publish to."
        >
          <input
            id="edge-token"
            type="password"
            value={token}
            onChange={(e) => setToken(e.target.value)}
            autoComplete="off"
            className="w-full rounded-md border border-border bg-surface px-3 py-2 font-mono text-sm"
          />
        </Field>

        <button
          onClick={() => test.mutate()}
          disabled={!accountID.trim() || !token || test.isPending}
          className="rounded-md border border-border px-3 py-2 text-sm hover:bg-surface-inset disabled:opacity-40"
        >
          {test.isPending ? 'Checking…' : 'Test connection'}
        </button>
      </div>

      {error && (
        <p role="alert" className="text-sm text-danger">
          {error}
        </p>
      )}

      {health && (
        <div className="space-y-3 rounded-md border border-border p-4">
          <p className="text-sm">
            <Check ok={health.reachable} label="Reachable" />{' '}
            <Check ok={health.authenticated} label="Authenticated" />
          </p>

          {health.missing_scopes.map((gap) => (
            <p key={gap.scope} className="text-sm text-warning">
              <span className="font-medium">{gap.scope}</span> is missing — {gap.blocks}
            </p>
          ))}
          {health.warnings.map((w) => (
            <p key={w} className="text-sm text-muted">
              {w}
            </p>
          ))}

          {usable.length > 0 && (
            <Field label="Tunnel" htmlFor="edge-tunnel">
              <select
                id="edge-tunnel"
                value={tunnelID}
                onChange={(e) => setTunnelID(e.target.value)}
                className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
              >
                <option value="">Choose a tunnel…</option>
                {usable.map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name} — {t.connections} connection{t.connections === 1 ? '' : 's'}
                  </option>
                ))}
              </select>
            </Field>
          )}

          {/* Shown rather than hidden: "my tunnel is missing" is a worse
              experience than being told why it cannot be used. */}
          {unusable.map((t) => (
            <UnusableTunnel key={t.id} tunnel={t} />
          ))}

          {health.zones.length > 0 && (
            <fieldset className="space-y-2">
              <legend className="text-sm font-medium">Zones this provider may write to</legend>
              <p className="text-xs text-muted">
                DNS permission reaches a whole zone, so this list is the real boundary. Nothing can
                be published to a zone that is not ticked, even though the token could reach it.
              </p>
              <div className="max-h-48 space-y-1 overflow-y-auto rounded-md border border-border p-2">
                {health.zones.map((zone) => (
                  <label key={zone.id} className="flex items-center gap-2 text-sm">
                    <input
                      type="checkbox"
                      checked={zoneIDs.includes(zone.id)}
                      onChange={(e) =>
                        setZoneIDs((current) =>
                          e.target.checked
                            ? [...current, zone.id]
                            : current.filter((id) => id !== zone.id),
                        )
                      }
                    />
                    {zone.name}
                  </label>
                ))}
              </div>
            </fieldset>
          )}

          <button
            onClick={() => save.mutate()}
            disabled={!tunnelID || zoneIDs.length === 0 || save.isPending}
            className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
          >
            {save.isPending ? 'Saving…' : 'Save'}
          </button>
          {tunnelID && zoneIDs.length === 0 && (
            <p className="text-xs text-muted">
              Tick at least one zone. A provider with none fails closed on every publish.
            </p>
          )}
        </div>
      )}
    </div>
  )
}

function UnusableTunnel({ tunnel }: { tunnel: EdgeTunnel }) {
  return (
    <p className="rounded-md border border-border bg-surface px-3 py-2 text-sm">
      <span className="font-medium">{tunnel.name}</span>{' '}
      <span className="text-muted">cannot be used here.</span>{' '}
      {tunnel.reason && <span className="text-muted">{tunnel.reason}</span>}
    </p>
  )
}

function Field({
  label,
  htmlFor,
  help,
  children,
}: {
  label: string
  htmlFor: string
  help?: string
  children: React.ReactNode
}) {
  return (
    <div className="space-y-1">
      <label htmlFor={htmlFor} className="block text-sm font-medium">
        {label}
      </label>
      {children}
      {help && <p className="text-xs text-muted">{help}</p>}
    </div>
  )
}

function Check({ ok, label }: { ok: boolean; label: string }) {
  return (
    <span className={ok ? 'text-running' : 'text-danger'}>
      {ok ? '✓' : '✕'} {label}
    </span>
  )
}
