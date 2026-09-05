import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { BuildTemplateBody, CatalogueImage, Platform } from '@/api/types'
import { Drawer } from '@/components/Drawer'

/** Build a cloud-init template from a published cloud image (ADR 0010).
 *
 *  This exists because the alternative was a sentence telling an operator to go
 *  and run four commands on a node — the same gap provisioning was built to
 *  close, one step earlier.
 *
 *  The node fetches the image, not the portal: the portal's egress is
 *  allow-listed to the cluster. That is also why the checksum has to be pasted
 *  rather than looked up — the portal cannot read the distribution's checksum
 *  file, so it links it instead.
 */
export function BuildTemplateForm({
  platform,
  onClose,
  onStarted,
}: {
  platform: Platform
  onClose: () => void
  onStarted: (requestID: string) => void
}) {
  const queryClient = useQueryClient()
  const [error, setError] = useState('')

  const catalogue = useQuery({
    queryKey: ['image-catalogue'],
    queryFn: () => api.get<{ data: CatalogueImage[] }>('/image-catalogue'),
    // The list ships with the build; it cannot go stale within a session.
    staleTime: Infinity,
  })

  const [imageID, setImageID] = useState('')
  const [customURL, setCustomURL] = useState('')
  const [name, setName] = useState('')
  const [node, setNode] = useState('')
  const [imageStorage, setImageStorage] = useState('local')
  const [diskStorage, setDiskStorage] = useState('local-lvm')
  const [bridge, setBridge] = useState('vmbr0')
  const [checksum, setChecksum] = useState('')
  const [skipChecksum, setSkipChecksum] = useState(false)

  const images = catalogue.data?.data ?? []
  const chosen = images.find((i) => i.id === imageID)
  const url = chosen?.url ?? customURL.trim()
  const custom = imageID === '' && customURL.trim() !== ''

  const build = useMutation({
    mutationFn: () => {
      const body: BuildTemplateBody = {
        name: name.trim(),
        node: node.trim(),
        image_url: url,
        image_storage: imageStorage.trim(),
        disk_storage: diskStorage.trim(),
        bridge: bridge.trim() || undefined,
        ...(skipChecksum
          ? { skip_checksum: true }
          : { checksum: checksum.trim(), checksum_algo: chosen?.checksum_algo ?? 'sha256' }),
      }
      return api.post<{ request_id: string; state: string }>(
        `/platforms/${platform.id}/templates`,
        body,
      )
    },
    onSuccess: (out) => {
      void queryClient.invalidateQueries({ queryKey: ['templates', platform.id] })
      onStarted(out.request_id)
      onClose()
    },
    onError: (err) =>
      setError(err instanceof Error ? err.message : 'The template could not be requested.'),
  })

  const ready =
    url !== '' &&
    name.trim() !== '' &&
    node.trim() !== '' &&
    imageStorage.trim() !== '' &&
    diskStorage.trim() !== '' &&
    (skipChecksum || checksum.trim() !== '')

  return (
    <Drawer
      title={`Build a template on ${platform.name}`}
      onClose={onClose}
      footer={
        <div className="flex items-center justify-between gap-3">
          {error && <span className="text-xs text-state-error">{error}</span>}
          <button
            onClick={() => build.mutate()}
            disabled={!ready || build.isPending}
            className="ml-auto rounded-md bg-accent px-3 py-1.5 text-sm text-white disabled:opacity-40"
          >
            {build.isPending ? 'Requesting…' : 'Build'}
          </button>
        </div>
      }
    >
      <div className="space-y-4 text-sm">
        <p className="rounded-md bg-surface-raised p-3 text-xs text-muted">
          The node downloads the image, imports it as a disk, attaches a cloud-init drive and
          converts the result. It takes a few minutes, mostly spent copying.
        </p>

        <label className="block">
          <span className="mb-1 block text-muted">Image</span>
          <select
            value={imageID}
            onChange={(e) => {
              setImageID(e.target.value)
              setChecksum('')
            }}
            className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
          >
            <option value="">A URL of my own…</option>
            {images.map((i) => (
              <option key={i.id} value={i.id}>
                {i.name}
              </option>
            ))}
          </select>
        </label>

        {imageID === '' && (
          <label className="block">
            <span className="mb-1 block text-muted">Image URL</span>
            <input
              value={customURL}
              onChange={(e) => setCustomURL(e.target.value)}
              placeholder="https://…/image.qcow2"
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 font-mono text-xs"
            />
          </label>
        )}

        {chosen && (
          <p className="-mt-2 text-xs text-muted">
            Logs in as <span className="font-mono text-fg">{chosen.login_user}</span>. Use that as
            the login user when provisioning from this template.
          </p>
        )}

        <div className="space-y-2 rounded-md border border-border p-3">
          <label className="block">
            <span className="mb-1 block text-xs text-muted">
              Checksum
              {chosen && (
                <>
                  {' — '}
                  <a
                    href={chosen.checksum_url}
                    target="_blank"
                    rel="noreferrer"
                    className="underline"
                  >
                    published here ({chosen.checksum_algo})
                  </a>
                </>
              )}
            </span>
            <input
              value={checksum}
              onChange={(e) => setChecksum(e.target.value)}
              disabled={skipChecksum}
              placeholder={custom ? 'digest for this file' : 'paste the digest for this file'}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5 font-mono text-xs disabled:opacity-40"
            />
          </label>
          <label className="flex items-start gap-2">
            <input
              type="checkbox"
              checked={skipChecksum}
              onChange={(e) => setSkipChecksum(e.target.checked)}
              className="mt-0.5"
            />
            <span className="text-xs">
              Build without verifying the image
              <span className="block text-muted">
                Every guest cloned from this template inherits it. Skipping is recorded in the audit
                log against your account.
              </span>
            </span>
          </label>
        </div>

        <label className="block">
          <span className="mb-1 block text-muted">Template name</span>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="debian-13-cloud"
            className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
          />
        </label>

        <div className="grid grid-cols-2 gap-3">
          <label className="block">
            <span className="mb-1 block text-muted">Node</span>
            <input
              value={node}
              onChange={(e) => setNode(e.target.value)}
              placeholder="pve"
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
            <span className="mb-1 block text-muted">Image storage</span>
            <input
              value={imageStorage}
              onChange={(e) => setImageStorage(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            />
          </label>
          <label className="block">
            <span className="mb-1 block text-muted">Disk storage</span>
            <input
              value={diskStorage}
              onChange={(e) => setDiskStorage(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-2 py-1.5"
            />
          </label>
        </div>
        <p className="-mt-2 text-xs text-muted">
          The downloaded file goes on a storage that accepts imports (usually a directory one like
          <span className="font-mono"> local</span>); the disk it becomes goes on one that holds
          images.
        </p>
      </div>
    </Drawer>
  )
}
