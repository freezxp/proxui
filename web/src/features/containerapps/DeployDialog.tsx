import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, detailOf } from '@/api/client'
import type {
  ContainerApp,
  ContainerAppUpstream,
  ContainerDeployment,
  Platform,
  StorageRow,
} from '@/api/types'
import { Drawer } from '@/components/Drawer'

/** Deploy one catalogue application into a container (ADR 0012).
 *
 *  The dialog shows the command before it runs, in full, because that is what
 *  the portal is asking permission for: a large third-party program, as root,
 *  on a hypervisor. Everything the operator can change reaches the node as an
 *  environment assignment — the request names an application by identifier and
 *  never a command — and the preview says so rather than implying the form
 *  writes a script.
 */
export function DeployDialog({
  app,
  upstream,
  onClose,
  onStarted,
}: {
  app: ContainerApp
  upstream?: ContainerAppUpstream
  onClose: () => void
  onStarted: (id: string) => void
}) {
  const queryClient = useQueryClient()
  const [platformID, setPlatformID] = useState('')
  const [node, setNode] = useState('')
  const [hostname, setHostname] = useState('')
  const [cores, setCores] = useState(app.cores ?? 0)
  const [memoryMB, setMemoryMB] = useState(app.memory_mb ?? 0)
  const [diskGB, setDiskGB] = useState(app.disk_gb ?? 0)
  const [storage, setStorage] = useState('')
  const [bridge, setBridge] = useState('')
  const [error, setError] = useState('')

  const platforms = useQuery({
    queryKey: ['platforms'],
    queryFn: () => api.get<{ data: Platform[] }>('/platforms'),
  })
  // Which nodes a platform has. The readiness report is what already knows,
  // and reusing it means one answer to "where can this go" rather than two.
  const readiness = useQuery({
    queryKey: ['readiness', platformID],
    queryFn: () => api.get<{ nodes: { node: string }[] }>(`/platforms/${platformID}/readiness`),
    enabled: platformID !== '',
  })
  const nodes = readiness.data?.nodes ?? []

  // The storage pools the chosen node actually has. Offered rather than typed
  // because a node with more than one and no answer is the one thing that makes
  // these scripts stop and ask — and a question asked on a node nobody is
  // watching ends the install with an exit status of zero.
  const storagePools = useQuery({
    queryKey: ['storage'],
    queryFn: () => api.get<{ data: StorageRow[] }>('/storage'),
  })
  const pools = (storagePools.data?.data ?? []).filter((s) => s.host_name === node)

  const deploy = useMutation({
    mutationFn: () =>
      api.post<ContainerDeployment>(`/platforms/${platformID}/container-deployments`, {
        app_id: app.id,
        node,
        hostname: hostname.trim() || undefined,
        cores: cores || undefined,
        memory_mb: memoryMB || undefined,
        disk_gb: diskGB || undefined,
        storage: storage.trim() || undefined,
        bridge: bridge.trim() || undefined,
      }),
    onSuccess: (started) => {
      void queryClient.invalidateQueries({ queryKey: ['container-deployments'] })
      onStarted(started.id)
      onClose()
    },
    onError: (err) => setError(detailOf(err, 'The deployment could not be started.')),
  })

  const ready = platformID !== '' && node !== ''

  return (
    <Drawer
      title={`Deploy ${app.name}`}
      onClose={onClose}
      footer={
        <div className="flex items-center justify-between gap-3">
          {error && <span className="text-xs text-danger">{error}</span>}
          <button
            onClick={() => deploy.mutate()}
            disabled={!ready || deploy.isPending}
            className="ml-auto rounded-md bg-accent px-3 py-1.5 text-sm text-white disabled:opacity-40"
          >
            {deploy.isPending ? 'Starting…' : `Deploy to ${node || 'a node'}`}
          </button>
        </div>
      }
    >
      <div className="space-y-4 text-sm">
        <p className="text-xs text-muted">
          This installs {app.name} into a new LXC container. The script is one of the community
          Proxmox VE Helper-Scripts, vendored with this portal at a reviewed commit; it runs as root
          on the node you pick.
          {app.source && (
            <>
              {' '}
              <a
                href={app.source}
                target="_blank"
                rel="noreferrer"
                className="text-accent underline"
              >
                About {app.name}
              </a>
            </>
          )}
        </p>

        <label className="block">
          <span className="mb-1 block text-muted">Platform</span>
          <select
            value={platformID}
            onChange={(e) => {
              setPlatformID(e.target.value)
              setNode('')
            }}
            className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
          >
            <option value="">Pick a platform…</option>
            {(platforms.data?.data ?? []).map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </select>
        </label>

        <label className="block">
          <span className="mb-1 block text-muted">Node</span>
          <select
            value={node}
            onChange={(e) => setNode(e.target.value)}
            disabled={platformID === ''}
            className="w-full rounded-md border border-border bg-surface px-2 py-1.5 disabled:opacity-40"
          >
            <option value="">{platformID === '' ? 'Pick a platform first' : 'Pick a node…'}</option>
            {nodes.map((n) => (
              <option key={n.node} value={n.node}>
                {n.node}
              </option>
            ))}
          </select>
        </label>

        <label className="block">
          <span className="mb-1 block text-muted">Hostname</span>
          <input
            value={hostname}
            onChange={(e) => setHostname(e.target.value)}
            placeholder={app.id}
            className="w-full rounded-md border border-border bg-surface px-2 py-1.5 font-mono text-xs"
          />
        </label>

        {/* Prefilled from the script's own defaults where it has them. Left at
            zero the portal sends nothing and the script decides, which is what
            has to happen for the ones that branch on container OS. */}
        <div className="grid grid-cols-3 gap-2">
          <Number label="Cores" value={cores} onChange={setCores} />
          <Number label="Memory (MB)" value={memoryMB} onChange={setMemoryMB} />
          <Number label="Disk (GB)" value={diskGB} onChange={setDiskGB} />
        </div>

        <div className="grid grid-cols-2 gap-2">
          <label className="block">
            <span className="mb-1 block text-muted">Storage</span>
            <select
              value={storage}
              onChange={(e) => setStorage(e.target.value)}
              disabled={node === ''}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 disabled:opacity-40"
            >
              <option value="">{node === '' ? 'Pick a node first' : "the node's default"}</option>
              {pools.map((p) => (
                <option key={p.id} value={p.name}>
                  {p.name}
                </option>
              ))}
            </select>
          </label>
          <label className="block">
            <span className="mb-1 block text-muted">Bridge</span>
            <input
              value={bridge}
              onChange={(e) => setBridge(e.target.value)}
              placeholder="vmbr0"
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 font-mono text-xs"
            />
          </label>
        </div>

        {node !== '' && pools.length > 1 && storage === '' && (
          <p className="rounded-md bg-paused/10 p-2 text-xs text-paused">
            {node} has {pools.length} storage pools and the script will ask which to use. Nobody is
            there to answer, so pick one here.
          </p>
        )}

        <div>
          <div className="mb-1 text-xs uppercase tracking-wide text-muted">What will run</div>
          <pre className="max-h-56 overflow-auto rounded-md bg-surface-inset p-2 font-mono text-[10px] leading-relaxed">
            {preview(app, { hostname, cores, memoryMB, diskGB, storage, bridge }, node, upstream)}
          </pre>
          <p className="mt-1 text-[11px] text-muted">
            Everything above is fixed by this portal except the values you set, which are passed as
            environment variables. Both upstream repositories are pinned to the commits shown.
          </p>
        </div>
      </div>
    </Drawer>
  )
}

function Number({
  label,
  value,
  onChange,
}: {
  label: string
  value: number
  onChange: (v: number) => void
}) {
  return (
    <label className="block">
      <span className="mb-1 block text-xs text-muted">{label}</span>
      <input
        type="number"
        min={0}
        value={value || ''}
        onChange={(e) => onChange(globalThis.Number(e.target.value) || 0)}
        placeholder="default"
        className="w-full rounded-md border border-border bg-surface px-2 py-1.5 tabular-nums"
      />
    </label>
  )
}

/** The command as the node will see it. Assembled here from the same pieces the
 *  server assembles it from, so what is shown is what runs rather than a
 *  friendly summary of it. */
function preview(
  app: ContainerApp,
  chosen: {
    hostname: string
    cores: number
    memoryMB: number
    diskGB: number
    storage: string
    bridge: string
  },
  node: string,
  upstream?: ContainerAppUpstream,
): string {
  const lines = [`# on ${node || '<node>'}, as root`, `export MODE=default`]
  if (upstream) {
    const raw = (repo: string, ref: string) => `https://raw.githubusercontent.com/${repo}/${ref}`
    lines.push(
      `export COMMUNITY_SCRIPTS_URL='${raw(upstream.scripts_repo, upstream.scripts_ref)}'`,
      `export COMMUNITY_SCRIPTS_CORE_URL='${raw(upstream.engine_repo, upstream.engine_ref)}'`,
    )
  }
  const set = (key: string, value: string | number) =>
    value ? lines.push(`export ${key}='${value}'`) : undefined
  set('var_hostname', chosen.hostname.trim())
  set('var_cpu', chosen.cores)
  set('var_ram', chosen.memoryMB)
  set('var_disk', chosen.diskGB)
  set('var_container_storage', chosen.storage.trim())
  set('var_brg', chosen.bridge.trim())
  lines.push(`bash ${app.id}.sh   # vendored with ProxUI`)
  return lines.join('\n')
}
