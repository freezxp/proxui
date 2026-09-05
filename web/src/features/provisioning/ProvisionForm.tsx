import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type {
  Paged,
  PlatformHealth,
  PortalKey,
  ProvisionBody,
  ProvisionRequest,
  Template,
  VMGroup,
} from '@/api/types'
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
  onClose,
  onStarted,
}: {
  onClose: () => void
  onStarted: (requestID: string) => void
}) {
  const queryClient = useQueryClient()
  const [error, setError] = useState('')

  // The form starts from the inventory rather than from one platform, so the
  // platform is the first thing it asks for: a template belongs to a cluster,
  // and everything after this depends on which one.
  const platforms = useQuery({
    queryKey: ['platforms'],
    queryFn: () => api.get<Paged<PlatformHealth>>('/platforms'),
    staleTime: 60_000,
  })
  const [platformID, setPlatformID] = useState('')

  const templates = useQuery({
    queryKey: ['templates', platformID],
    queryFn: () => api.get<{ data: Template[] }>(`/platforms/${platformID}/templates`),
    enabled: platformID !== '',
  })

  // Filing the guest into a VM group is the difference between a machine only
  // administrators can see and one its owners can. The API has taken this since
  // provisioning shipped and nothing offered it.
  const groups = useQuery({
    queryKey: ['vm-groups'],
    queryFn: () => api.get<{ data: VMGroup[] }>('/vm-groups'),
    staleTime: 60_000,
  })
  const portalKey = useQuery({
    queryKey: ['portal-key'],
    queryFn: () => api.get<PortalKey>('/ssh-key'),
  })

  const [templateID, setTemplateID] = useState('')
  const [name, setName] = useState('')
  const [node, setNode] = useState('')
  const [groupID, setGroupID] = useState('')
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

  const platformList = platforms.data?.data ?? []
  const platform = platformList.find((p) => p.id === platformID)
  const list = templates.data?.data ?? []
  const chosen = list.find((t) => t.external_id === templateID)
  // A full clone can land on another node; the template's own is the sensible
  // default and the only one guaranteed to work for a linked clone.
  const targetNode = node.trim() || chosen?.node || ''
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
        node: targetNode,
        vm_group_id: groupID || undefined,
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
        `/platforms/${platformID}/provision`,
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

  const ready = platformID !== '' && templateID !== '' && name.trim() !== '' && targetNode !== ''

  return (
    <Drawer
      title="Create a VM"
      onClose={onClose}
      footer={
        <div className="flex items-center justify-between gap-3">
          {error && <span className="text-xs text-danger">{error}</span>}
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
        <label className="block">
          <span className="mb-1 block text-muted">Platform</span>
          <select
            value={platformID}
            onChange={(e) => {
              setPlatformID(e.target.value)
              // Everything below belongs to whichever platform was chosen.
              setTemplateID('')
              setNode('')
            }}
            className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
          >
            <option value="">Choose one…</option>
            {platformList.map((p) => (
              <option key={p.id} value={p.id}>
                {p.name}
              </option>
            ))}
          </select>
        </label>

        {platformID === '' ? (
          <p className="text-muted">Pick a platform to see what it can be cloned from.</p>
        ) : templates.isLoading ? (
          <p className="text-muted">Loading templates…</p>
        ) : list.length === 0 ? (
          <p className="rounded-md bg-surface-raised p-3 text-muted">
            {platform?.name} has no templates yet. Build one from its page under Platforms — the
            node downloads a cloud image, imports it, attaches a cloud-init drive and converts the
            result.
          </p>
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
          <p className="rounded-md bg-paused/10 p-3 text-xs text-paused">
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

        <div className="grid grid-cols-2 gap-3">
          <label className="block">
            <span className="mb-1 block text-muted">Node</span>
            <input
              value={targetNode}
              onChange={(e) => setNode(e.target.value)}
              placeholder={chosen?.node ?? 'the template’s node'}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            />
          </label>
          <label className="block">
            <span className="mb-1 block text-muted">VM group</span>
            <select
              value={groupID}
              onChange={(e) => setGroupID(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            >
              <option value="">None</option>
              {(groups.data?.data ?? []).map((g) => (
                <option key={g.id} value={g.id}>
                  {g.name}
                </option>
              ))}
            </select>
          </label>
        </div>
        <p className="-mt-2 text-xs text-muted">
          A guest in no group is visible only to administrators — groups are what grants reach.
          Moving to another node needs the template on shared storage.
        </p>

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
    // Stops polling once there is nothing left to happen. Waiting for a guest
    // to answer is minutes rather than seconds, so it is asked about less
    // often — a first boot does not become quicker for being watched.
    refetchInterval: (query) => {
      const state = query.state.data?.state
      if (!state) return 3000
      if (['ready', 'deleted', 'failed'].includes(state)) return false
      return state === 'verifying' ? 10000 : 3000
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
          className={data.state === 'failed' ? 'text-danger' : done ? 'text-running' : 'text-muted'}
        >
          {data.state === 'verifying' ? 'waiting for the guest' : data.state}
        </span>
      </div>
      {data.vmid && <p className="mt-1 font-mono text-xs text-muted">VMID {data.vmid}</p>}
      {data.state === 'verifying' && (
        <p className="mt-1 text-xs text-muted">
          The guest is up on the platform. This waits for its agent, which is what proves it
          actually booted rather than that the platform accepted every call.
        </p>
      )}
      {data.error && (
        // A finished request can carry a note rather than a failure: a template
        // built without a guest agent, or a guest that never answered. Both are
        // worth reading and neither is red.
        <p className={`mt-2 text-xs ${data.state === 'failed' ? 'text-danger' : 'text-paused'}`}>
          {data.step && data.state === 'failed' && (
            <span className="font-medium">{data.step}: </span>
          )}
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
