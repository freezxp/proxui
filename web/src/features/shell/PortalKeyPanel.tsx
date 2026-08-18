import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, detailOf } from '@/api/client'
import type { PortalKey, PortalKeyInstall } from '@/api/types'
import { copyText } from '@/lib/clipboard'

/**
 * The portal's own SSH key (SSH-11..SSH-14, ADR 0006).
 *
 * The panel exists to make three things obvious, because each of them is a
 * decision somebody has to make with their eyes open: that the portal holds a
 * key at all, exactly which guests and accounts it opens, and that rotating it
 * breaks every one of those at once.
 *
 * The public half is shown in full and is meant to be copied — pasting it into
 * a cloud-init template is the other supported way to install it, and the one
 * that works on a guest nobody has a password for yet.
 */
export function PortalKeyPanel() {
  const queryClient = useQueryClient()
  const [error, setError] = useState('')
  const [confirming, setConfirming] = useState<'rotate' | 'delete' | null>(null)
  const [copied, setCopied] = useState(false)

  const key = useQuery({
    queryKey: ['ssh-key'],
    queryFn: () => api.get<PortalKey>('/ssh-key'),
  })
  const installs = useQuery({
    queryKey: ['ssh-key-installs'],
    queryFn: () => api.get<{ data: PortalKeyInstall[] }>('/ssh-key/installs'),
  })

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: ['ssh-key'] })
    void queryClient.invalidateQueries({ queryKey: ['ssh-key-installs'] })
  }
  const fail = (err: unknown, fallback: string) => setError(detailOf(err, fallback))

  const generate = useMutation({
    mutationFn: () => api.post<PortalKey>('/ssh-key', {}),
    onSuccess: () => {
      setError('')
      setConfirming(null)
      refresh()
    },
    onError: (err) => fail(err, 'The key could not be generated.'),
  })

  const remove = useMutation({
    mutationFn: () => api.del('/ssh-key'),
    onSuccess: () => {
      setError('')
      setConfirming(null)
      refresh()
    },
    onError: (err) => fail(err, 'The key could not be deleted.'),
  })

  const busy = generate.isPending || remove.isPending
  const current = key.data
  const rows = installs.data?.data ?? []
  const staleCount = rows.filter((row) => row.stale).length

  return (
    <section className="space-y-4 rounded-lg border border-border p-4">
      <div>
        <h2 className="text-sm font-medium">Portal SSH key</h2>
        <p className="mt-1 text-xs text-muted">
          One key pair the portal holds. Install its public half into an account&rsquo;s
          authorized_keys on a guest and operators stop typing that account&rsquo;s password. The
          private half is sealed in the vault and never leaves the server; guest passwords are still
          never stored.
        </p>
      </div>

      {error !== '' && (
        <p className="text-sm text-danger" role="alert">
          {error}
        </p>
      )}

      {key.isLoading ? (
        <p className="text-sm text-muted">Loading…</p>
      ) : current?.exists !== true ? (
        <div className="space-y-2">
          <p className="text-sm text-muted">No key has been generated yet.</p>
          <button
            type="button"
            onClick={() => generate.mutate()}
            disabled={busy}
            className="rounded-md bg-accent px-3 py-1.5 text-sm font-medium text-white disabled:opacity-50"
          >
            Generate key
          </button>
        </div>
      ) : (
        <div className="space-y-3">
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
            <dt className="text-muted">Fingerprint</dt>
            <dd className="font-mono break-all">{current.fingerprint}</dd>
            <dt className="text-muted">Algorithm</dt>
            <dd className="font-mono">{current.algorithm}</dd>
            <dt className="text-muted">Created</dt>
            <dd>{current.created_at ? new Date(current.created_at).toLocaleString() : '—'}</dd>
          </dl>

          <div className="space-y-1">
            <div className="flex items-center gap-2">
              <span className="text-xs text-muted">Public key</span>
              <button
                type="button"
                onClick={() => {
                  void copyText(current.public_key ?? '').then((ok) => setCopied(ok))
                }}
                className="rounded-md border border-border px-2 py-0.5 text-xs hover:bg-surface-raised"
              >
                {copied ? 'Copied' : 'Copy'}
              </button>
            </div>
            <pre className="overflow-x-auto rounded-md border border-border bg-surface-raised p-2 font-mono text-xs break-all whitespace-pre-wrap">
              {current.public_key}
            </pre>
          </div>

          <div className="flex flex-wrap gap-2">
            <button
              type="button"
              onClick={() => setConfirming('rotate')}
              disabled={busy}
              className="rounded-md border border-border px-3 py-1.5 text-sm hover:bg-surface-raised disabled:opacity-50"
            >
              Rotate
            </button>
            <button
              type="button"
              onClick={() => setConfirming('delete')}
              disabled={busy}
              className="rounded-md border border-danger/50 px-3 py-1.5 text-sm text-danger disabled:opacity-50"
            >
              Delete
            </button>
          </div>

          {confirming !== null && (
            // Both consequences are stated in full, because neither is
            // reversible and neither is visible from here: the lines are on
            // machines this page cannot reach.
            <div className="space-y-2 rounded-md border border-danger/50 p-3 text-xs">
              <p>
                {confirming === 'rotate'
                  ? 'Rotating replaces the key. Every guest still carrying the old line stops accepting it, and each one has to be installed again.'
                  : 'Deleting removes the key from the portal. The lines already in authorized_keys on guests are left behind and have to be removed by hand.'}
                {rows.length > 0 &&
                  ` ${rows.length} install${rows.length === 1 ? '' : 's'} affected.`}
              </p>
              <div className="flex gap-2">
                <button
                  type="button"
                  onClick={() => (confirming === 'rotate' ? generate.mutate() : remove.mutate())}
                  disabled={busy}
                  className="rounded-md bg-danger px-3 py-1 text-white disabled:opacity-50"
                >
                  {confirming === 'rotate' ? 'Rotate the key' : 'Delete the key'}
                </button>
                <button
                  type="button"
                  onClick={() => setConfirming(null)}
                  className="rounded-md border border-border px-3 py-1 hover:bg-surface-raised"
                >
                  Cancel
                </button>
              </div>
            </div>
          )}
        </div>
      )}

      <div className="space-y-2">
        <h3 className="text-xs font-medium text-muted">
          Installed on {rows.length} account{rows.length === 1 ? '' : 's'}
          {staleCount > 0 && ` · ${staleCount} stale`}
        </h3>
        {rows.length === 0 ? (
          <p className="text-xs text-muted">
            Nowhere yet. Open an SSH session with a password and use Install portal key.
          </p>
        ) : (
          <ul className="divide-y divide-border text-xs">
            {rows.map((row) => (
              <li key={`${row.vm_id}/${row.ssh_user}`} className="flex flex-wrap gap-2 py-1.5">
                <span className="min-w-40 font-medium">{row.vm_name ?? row.vm_id}</span>
                <span className="font-mono text-muted">{row.ssh_user}</span>
                <span className="ml-auto text-muted">
                  {new Date(row.installed_at).toLocaleDateString()}
                </span>
                {row.stale && (
                  <span className="rounded-full bg-danger/10 px-2 py-0.5 text-danger">stale</span>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  )
}
