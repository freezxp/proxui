import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { HostRow, Platform, PortalKey } from '@/api/types'
import { copyText } from '@/lib/clipboard'

/**
 * How to get hardware readings out of this platform's nodes.
 *
 * It lives on the platform rather than in the documentation because the two
 * things an operator needs — the portal's actual public key, and which of
 * their nodes are still not answering — are specific to this deployment and
 * cannot be written down in advance. The prose is the short version of
 * docs/30-node-sensors.md.
 */
export function MonitoringGuide({ platform }: { platform: Platform }) {
  const key = useQuery({
    queryKey: ['portal-key'],
    queryFn: () => api.get<PortalKey>('/ssh-key'),
  })
  const hosts = useQuery({
    queryKey: ['hosts'],
    queryFn: () => api.get<{ data: HostRow[] }>('/hosts'),
    refetchInterval: 60_000,
  })

  const nodes = (hosts.data?.data ?? []).filter((h) => h.platform_id === platform.id)
  const answering = nodes.filter((n) => (n.sensors?.count ?? 0) > 0)
  const publicKey = (key.data?.exists && key.data.public_key) || ''

  return (
    <div className="space-y-3">
      <div>
        <h3 className="text-sm font-medium">Monitoring node hardware</h3>
        <p className="mt-1 text-xs text-muted">
          Proxmox publishes no temperature in its API — not at a different privilege, not on a newer
          version. The portal reads it from each node instead: one SSH connection every five
          minutes, running <code>sensors -j</code> and nothing else, with the key below. No node
          password is stored.
        </p>
      </div>

      <NodeStatus nodes={nodes} loading={hosts.isLoading} />

      {answering.length < nodes.length && (
        <Setup publicKey={publicKey} keyMissing={key.isFetched && !key.data?.exists} />
      )}
    </div>
  )
}

/** Which nodes answer and which do not, with the reason attached to each. */
function NodeStatus({ nodes, loading }: { nodes: HostRow[]; loading: boolean }) {
  if (loading) return <p className="text-xs text-muted">Checking nodes…</p>
  if (nodes.length === 0) {
    return (
      <p className="text-xs text-muted">No nodes have been synchronized from this platform yet.</p>
    )
  }

  return (
    <ul className="divide-y divide-border rounded-md border border-border text-xs">
      {nodes.map((node) => {
        const count = node.sensors?.count ?? 0
        return (
          <li key={node.id} className="flex items-baseline justify-between gap-3 px-3 py-2">
            <span className="font-medium">{node.name}</span>
            {count > 0 ? (
              <span className="text-running">
                {count} sensors
                {node.sensors?.hottest && (
                  <span className="ml-2 text-muted">
                    hottest {Math.round(node.sensors.hottest.value)}°C ·{' '}
                    {node.sensors.hottest.label}
                  </span>
                )}
              </span>
            ) : (
              <span className="text-right text-muted">
                {node.sensor_error || (node.sensors_ever_read ? 'no readings' : 'not read yet')}
              </span>
            )}
          </li>
        )
      })}
    </ul>
  )
}

/** The two commands, on the node, in the order they have to happen. */
function Setup({ publicKey, keyMissing }: { publicKey: string; keyMissing: boolean }) {
  const install = publicKey
    ? `mkdir -p /root/.ssh && chmod 700 /root/.ssh\necho '${publicKey}' >> /root/.ssh/authorized_keys\nchmod 600 /root/.ssh/authorized_keys`
    : ''

  return (
    <div className="space-y-3 rounded-md border border-border p-3">
      <Step
        n={1}
        title="Install lm-sensors on the node"
        body="The kernel already reads the hardware; this is what prints it. If sensors -j comes back empty, the board has no sensors the kernel recognises — common on a virtualised node, and nothing the portal can do about it."
        command={'apt install lm-sensors\nsensors-detect --auto\nsensors -j'}
      />
      <Step
        n={2}
        title="Install the portal's public key for root"
        body="The portal authenticates as itself, so there is no node password to store anywhere. Taking the access back is deleting this line."
        command={install}
      >
        {keyMissing && (
          <p className="text-xs text-warning">
            This portal has no SSH key yet. Generate one in <strong>Settings → SSH key</strong>{' '}
            first — the same key is used for guest sessions.
          </p>
        )}
      </Step>
      <p className="text-xs text-muted">
        Readings appear on <strong>Hosts</strong> within five minutes; click a node there to see
        every sensor. Alert rules with a <strong>node</strong> subject can watch the temperature, or
        the headroom left to the chip&apos;s own critical point.
      </p>
    </div>
  )
}

function Step({
  n,
  title,
  body,
  command,
  children,
}: {
  n: number
  title: string
  body: string
  command: string
  children?: React.ReactNode
}) {
  const [copied, setCopied] = useState(false)

  return (
    <div className="space-y-1.5">
      <div className="flex items-baseline gap-2">
        <span className="flex h-4 w-4 shrink-0 items-center justify-center rounded-full bg-accent/15 text-[10px] font-medium text-accent">
          {n}
        </span>
        <h4 className="text-xs font-medium">{title}</h4>
      </div>
      <p className="pl-6 text-xs text-muted">{body}</p>
      {children && <div className="pl-6">{children}</div>}
      {command && (
        <div className="relative pl-6">
          <pre className="overflow-x-auto rounded border border-border bg-surface p-2 pr-16 font-mono text-[11px] leading-relaxed">
            {command}
          </pre>
          <button
            type="button"
            onClick={async () => {
              setCopied(await copyText(command))
              setTimeout(() => setCopied(false), 2000)
            }}
            className="absolute right-1.5 top-1.5 rounded border border-border bg-surface-raised px-1.5 py-0.5 text-[10px] text-muted hover:text-content"
          >
            {copied ? 'Copied' : 'Copy'}
          </button>
        </div>
      )}
    </div>
  )
}
