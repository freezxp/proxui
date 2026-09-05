import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { Platform, PortalKey, ProvisionBody, ProvisionRequest, Template } from '@/api/types'
import { Drawer } from '@/components/Drawer'

/** Create a guest from a cloud-init template (ADR 0010).
 *
 *  The form asks for a user name and SSH keys and has no password field, which
 *  is the same decision the API and the connector make: cloud-init takes keys,
 *  and a password would be a secret passing through four places on its way to
 *  the platform. The portal's own key is offered first because a guest it can
 *  reach over SSH is one whose console and file browser work immediately.
 */
export function ProvisionForm({
  platform,
  onClose,
  onStarted,
  onBuildTemplate,
}: {
  platform: Platform
  onClose: () => void
  onStarted: (requestID: string) => void
  /** Opens the template builder, for the case this form cannot proceed without
   *  one. Offering the way forward beats describing it. */
  onBuildTemplate: () => void
}) {
  const queryClient = useQueryClient()
  const [error, setError] = useState('')

  const templates = useQuery({
    queryKey: ['templates', platform.id],
    queryFn: () => api.get<{ data: Template[] }>(`/platforms/${platform.id}/templates`),
  })
  const portalKey = useQuery({
    queryKey: ['portal-key'],
    queryFn: () => api.get<PortalKey>('/ssh-key'),
  })

  const [templateID, setTemplateID] = useState('')
  const [name, setName] = useState('')
  const [storage, setStorage] = useState('')
  const [cores, setCores] = useState(2)
  const [memoryMB, setMemoryMB] = useState(2048)
  const [diskGrowGB, setDiskGrowGB] = useState(0)
  const [bridge, setBridge] = useState('vmbr0')
  const [ciUser, setCIUser] = useState('ubuntu')
  const [ipConfig, setIPConfig] = useState('ip=dhcp')
  const [extraKeys, setExtraKeys] = useState('')
  const [usePortalKey, setUsePortalKey] = useState(true)
  const [startAfter, setStartAfter] = useState(true)

  const list = templates.data?.data ?? []
  const chosen = list.find((t) => t.external_id === templateID)
  const portalPublicKey = (portalKey.data?.exists && portalKey.data.public_key) || ''

  const create = useMutation({
    mutationFn: () => {
      const keys = [
        ...(usePortalKey && portalPublicKey ? [portalPublicKey] : []),
        ...extraKeys
          .split('\n')
          .map((k) => k.trim())
          .filter(Boolean),
      ]
      const body: ProvisionBody = {
        template_id: templateID,
        name: name.trim(),
        node: chosen?.node ?? '',
        storage: storage.trim() || undefined,
        full_clone: true,
        ci_user: ciUser.trim() || undefined,
        ssh_keys: keys.length > 0 ? keys : undefined,
        ip_config: ipConfig.trim() || undefined,
        cores,
        memory_mb: memoryMB,
        bridge: bridge.trim() || undefined,
        // Only sent when it is a real growth: the platform rejects a resize of
        // zero, and disks cannot shrink.
        disk_name: diskGrowGB > 0 ? 'scsi0' : undefined,
        disk_grow_gb: diskGrowGB > 0 ? diskGrowGB : undefined,
        start_after_create: startAfter,
      }
      return api.post<{ request_id: string; state: string }>(
        `/platforms/${platform.id}/provision`,
        body,
      )
    },
    onSuccess: (out) => {
      void queryClient.invalidateQueries({ queryKey: ['provision-requests'] })
      onStarted(out.request_id)
      onClose()
    },
    onError: (err) =>
      setError(err instanceof Error ? err.message : 'The guest could not be requested.'),
  })

  const ready = templateID !== '' && name.trim() !== ''

  return (
    <Drawer
      title={`New guest on ${platform.name}`}
      onClose={onClose}
      footer={
        <div className="flex items-center justify-between gap-3">
          {error && <span className="text-xs text-state-error">{error}</span>}
          <button
            onClick={() => create.mutate()}
            disabled={!ready || create.isPending}
            className="ml-auto rounded-md bg-accent px-3 py-1.5 text-sm text-white disabled:opacity-40"
          >
            {create.isPending ? 'Requesting…' : 'Create'}
          </button>
        </div>
      }
    >
      <div className="space-y-4 text-sm">
        {templates.isLoading ? (
          <p className="text-muted">Loading templates…</p>
        ) : list.length === 0 ? (
          <div className="space-y-2 rounded-md bg-surface-raised p-3">
            <p className="text-muted">
              This platform has no templates yet. The portal can build one: it has the node download
              a cloud image, import it, attach a cloud-init drive and convert the result.
            </p>
            <button
              onClick={onBuildTemplate}
              className="rounded-md border border-border px-2 py-1 text-xs"
            >
              Build one
            </button>
          </div>
        ) : (
          <label className="block">
            <span className="mb-1 block text-muted">Template</span>
            <select
              value={templateID}
              onChange={(e) => setTemplateID(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            >
              <option value="">Choose one…</option>
              {list.map((t) => (
                <option key={t.external_id} value={t.external_id}>
                  {t.name} ({t.node})
                </option>
              ))}
            </select>
          </label>
        )}

        {chosen && !chosen.has_cloud_init && (
          <p className="rounded-md bg-state-paused/10 p-3 text-xs text-state-paused">
            This template has no cloud-init drive. The user name and SSH keys below cannot be
            applied to it, and the guest will start with whatever credentials the image was built
            with.
          </p>
        )}

        <label className="block">
          <span className="mb-1 block text-muted">Name</span>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="web-02"
            className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
          />
        </label>

        <div className="grid grid-cols-3 gap-3">
          <label className="block">
            <span className="mb-1 block text-muted">Cores</span>
            <input
              type="number"
              min={1}
              value={cores}
              onChange={(e) => setCores(Number(e.target.value))}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            />
          </label>
          <label className="block">
            <span className="mb-1 block text-muted">Memory (MB)</span>
            <input
              type="number"
              min={256}
              step={256}
              value={memoryMB}
              onChange={(e) => setMemoryMB(Number(e.target.value))}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            />
          </label>
          <label className="block">
            <span className="mb-1 block text-muted">Grow disk (GB)</span>
            <input
              type="number"
              min={0}
              value={diskGrowGB}
              onChange={(e) => setDiskGrowGB(Number(e.target.value))}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            />
          </label>
        </div>
        <p className="-mt-2 text-xs text-muted">
          Disks can only be grown, and only before first boot — a disk enlarged afterwards is one
          the filesystem inside does not know about.
        </p>

        <div className="grid grid-cols-2 gap-3">
          <label className="block">
            <span className="mb-1 block text-muted">Storage</span>
            <input
              value={storage}
              onChange={(e) => setStorage(e.target.value)}
              placeholder="platform default"
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            />
          </label>
          <label className="block">
            <span className="mb-1 block text-muted">Bridge</span>
            <input
              value={bridge}
              onChange={(e) => setBridge(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            />
          </label>
        </div>

        <div className="grid grid-cols-2 gap-3">
          <label className="block">
            <span className="mb-1 block text-muted">Login user</span>
            <input
              value={ciUser}
              onChange={(e) => setCIUser(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            />
          </label>
          <label className="block">
            <span className="mb-1 block text-muted">Network</span>
            <input
              value={ipConfig}
              onChange={(e) => setIPConfig(e.target.value)}
              placeholder="ip=dhcp"
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 font-mono text-xs"
            />
          </label>
        </div>

        <div className="space-y-2 rounded-md bg-surface-raised p-3">
          <p className="text-xs text-muted">
            Access is by SSH key. The portal never asks for, carries, or stores a guest password.
          </p>
          <label className="flex items-start gap-2">
            <input
              type="checkbox"
              checked={usePortalKey}
              onChange={(e) => setUsePortalKey(e.target.checked)}
              disabled={!portalPublicKey}
              className="mt-0.5"
            />
            <span className="text-xs">
              Install the portal&rsquo;s own key
              {!portalPublicKey && ' — none generated yet'}
              <span className="block text-muted">
                Lets the browser terminal and file browser reach the guest without anyone typing a
                credential.
              </span>
            </span>
          </label>
          <label className="block">
            <span className="mb-1 block text-xs text-muted">Additional keys, one per line</span>
            <textarea
              value={extraKeys}
              onChange={(e) => setExtraKeys(e.target.value)}
              rows={3}
              placeholder="ssh-ed25519 AAAA… you@laptop"
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 font-mono text-xs"
            />
          </label>
        </div>

        <label className="flex items-center gap-2">
          <input
            type="checkbox"
            checked={startAfter}
            onChange={(e) => setStartAfter(e.target.checked)}
          />
          <span className="text-xs">Start the guest once it is built</span>
        </label>
      </div>
    </Drawer>
  )
}

/** Live status of one request, polled until it settles. */
export function ProvisionStatus({ requestID }: { requestID: string }) {
  const request = useQuery({
    queryKey: ['provision-request', requestID],
    queryFn: () => api.get<ProvisionRequest>(`/provision-requests/${requestID}`),
    // Stops polling once there is nothing left to happen.
    refetchInterval: (query) => {
      const state = query.state.data?.state
      return state && ['ready', 'deleted', 'failed'].includes(state) ? false : 3000
    },
  })

  const data = request.data
  if (!data) return null

  const done = data.state === 'ready' || data.state === 'deleted'
  return (
    <div className="rounded-md border border-border p-3 text-sm">
      <div className="flex items-center justify-between">
        <span className="font-medium">{data.guest_name}</span>
        <span
          className={
            data.state === 'failed' ? 'text-state-error' : done ? 'text-state-ok' : 'text-muted'
          }
        >
          {data.state}
        </span>
      </div>
      {data.vmid && <p className="mt-1 font-mono text-xs text-muted">VMID {data.vmid}</p>}
      {data.error && (
        <p className="mt-2 text-xs text-state-error">
          {data.step && <span className="font-medium">{data.step}: </span>}
          {data.error}
        </p>
      )}
      {data.state === 'failed' && data.vmid && (
        <p className="mt-2 text-xs text-muted">
          The guest was left in place rather than cleaned up automatically. Check it on the platform
          before removing it.
        </p>
      )}
    </div>
  )
}
