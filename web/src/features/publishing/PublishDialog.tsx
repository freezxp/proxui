import { useMemo, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { api, ApiError } from '@/api/client'
import type { Paged, PublishedApp, VMListItem } from '@/api/types'

type TargetMode = 'vm' | 'address'

/**
 * Publishing an app.
 *
 * The VM picker is the default because it is the only thing this panel does
 * that Cloudflare's own dashboard cannot: the portal knows which machine
 * 10.0.29.177 is, and can keep the rule and the machine in step afterwards.
 * A free-text address stays available for anything outside the inventory.
 */
export function PublishDialog({
  providerID,
  version,
  onClose,
  onPublished,
}: {
  providerID: string
  /** The version the routing table was read at, so a concurrent change is refused. */
  version: number
  onClose: () => void
  onPublished: () => void
}) {
  const [mode, setMode] = useState<TargetMode>('vm')
  const [vmID, setVMID] = useState('')
  const [address, setAddress] = useState('')
  const [scheme, setScheme] = useState('http')
  const [port, setPort] = useState('')
  const [hostname, setHostname] = useState('')
  const [acknowledged, setAcknowledged] = useState(false)
  const [error, setError] = useState('')

  const vms = useQuery({
    queryKey: ['vms', 'publishable'],
    queryFn: () => api.get<Paged<VMListItem>>('/vms?page_size=500'),
    enabled: mode === 'vm',
  })

  // Only machines with an address can be published: the rule needs somewhere
  // to point. Saying so beside the picker beats an empty list with no reason.
  const { withAddress, withoutAddress } = useMemo(() => {
    const rows = vms.data?.data ?? []
    return {
      withAddress: rows.filter((vm) => vm.ip_addresses?.length > 0),
      withoutAddress: rows.filter((vm) => !vm.ip_addresses?.length),
    }
  }, [vms.data])

  const chosenVM = withAddress.find((vm) => vm.id === vmID)
  const resolvedAddress = mode === 'vm' ? (chosenVM?.ip_addresses[0] ?? '') : address.trim()
  const ready = hostname.trim() !== '' && resolvedAddress !== '' && port.trim() !== ''

  const body = () => ({
    hostname: hostname.trim(),
    scheme,
    address: resolvedAddress,
    port: Number(port),
    vm_id: mode === 'vm' ? vmID : '',
    acknowledge_exposure: acknowledged,
    read_version: version,
  })

  // There is deliberately no client-side preview here. Producing one would
  // mean rebuilding the desired routing table in TypeScript — reimplementing
  // the domain's insert-before-catch-all rule — and a second implementation of
  // an invariant is a second chance to get it wrong. The publish call itself
  // reads live, validates the whole table and refuses precisely, so the answer
  // it gives is the one that counts. The /preview endpoint exists for the
  // whole-table editor, where the client legitimately holds the table.

  const publish = useMutation({
    mutationFn: () => api.post<PublishedApp>(`/edge-providers/${providerID}/apps`, body()),
    onSuccess: onPublished,
    onError: (err) => setError(publishError(err)),
  })

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center p-4">
      <div className="absolute inset-0 bg-black/40" onClick={onClose} aria-hidden="true" />
      <div
        role="dialog"
        aria-modal="true"
        aria-label="Publish an app"
        className="relative max-h-full w-full max-w-2xl space-y-4 overflow-y-auto rounded-lg border border-border bg-surface-raised p-6"
      >
        <div>
          <h2 className="text-lg font-semibold">Publish an app</h2>
          <p className="text-sm text-muted">
            Gives a service a public hostname through this tunnel. Both the routing rule and the DNS
            record are created.
          </p>
        </div>

        <fieldset className="space-y-2">
          <legend className="text-sm font-medium">What are you publishing?</legend>
          <div className="flex gap-2">
            <ModeButton active={mode === 'vm'} onClick={() => setMode('vm')}>
              A VM in the inventory
            </ModeButton>
            <ModeButton active={mode === 'address'} onClick={() => setMode('address')}>
              An address
            </ModeButton>
          </div>
        </fieldset>

        {mode === 'vm' ? (
          <div className="space-y-2">
            <label htmlFor="publish-vm" className="block text-sm font-medium">
              Virtual machine
            </label>
            {vms.isLoading ? (
              <p className="text-sm text-muted">Loading the inventory…</p>
            ) : (
              <>
                <select
                  id="publish-vm"
                  value={vmID}
                  onChange={(e) => setVMID(e.target.value)}
                  className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
                >
                  <option value="">Choose a VM…</option>
                  {withAddress.map((vm) => (
                    <option key={vm.id} value={vm.id}>
                      {vm.name} — {vm.ip_addresses[0]} ({vm.state})
                    </option>
                  ))}
                </select>
                {withoutAddress.length > 0 && (
                  <p className="text-xs text-muted">
                    {withoutAddress.length} VM{withoutAddress.length === 1 ? '' : 's'}{' '}
                    {withoutAddress.length === 1 ? 'is' : 'are'} not listed because the portal has
                    no address for {withoutAddress.length === 1 ? 'it' : 'them'}. Proxmox reports a
                    VM&apos;s address through its guest agent; without one running there is nothing
                    to point a rule at.
                  </p>
                )}
                {chosenVM && chosenVM.ip_addresses.length > 1 && (
                  <p className="text-xs text-muted">
                    This VM has several addresses; {chosenVM.ip_addresses[0]} is used.
                  </p>
                )}
              </>
            )}
          </div>
        ) : (
          <div className="space-y-2">
            <label htmlFor="publish-address" className="block text-sm font-medium">
              Address
            </label>
            <input
              id="publish-address"
              value={address}
              onChange={(e) => setAddress(e.target.value)}
              placeholder="10.0.13.9"
              className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
            />
            <p className="text-xs text-muted">
              For anything the portal does not sync. The tunnel has to be able to reach it.
            </p>
          </div>
        )}

        <div className="grid grid-cols-3 gap-3">
          <div className="space-y-1">
            <label htmlFor="publish-scheme" className="block text-sm font-medium">
              Scheme
            </label>
            <select
              id="publish-scheme"
              value={scheme}
              onChange={(e) => setScheme(e.target.value)}
              className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
            >
              <option value="http">http</option>
              <option value="https">https</option>
            </select>
          </div>
          <div className="space-y-1">
            <label htmlFor="publish-port" className="block text-sm font-medium">
              Port
            </label>
            <input
              id="publish-port"
              value={port}
              onChange={(e) => setPort(e.target.value.replace(/\D/g, ''))}
              inputMode="numeric"
              placeholder="8080"
              className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
            />
          </div>
          <div className="col-span-3 space-y-1">
            <label htmlFor="publish-hostname" className="block text-sm font-medium">
              Public hostname
            </label>
            <input
              id="publish-hostname"
              value={hostname}
              onChange={(e) => setHostname(e.target.value)}
              placeholder="app.example.com"
              className="w-full rounded-md border border-border bg-surface px-3 py-2 text-sm"
            />
            <p className="text-xs text-muted">
              Must be in a zone this provider is allowed to write to.
            </p>
          </div>
        </div>

        {resolvedAddress && port && (
          <p className="rounded-md border border-border bg-surface px-3 py-2 font-mono text-xs">
            {hostname.trim() || 'hostname'} → {scheme}://{resolvedAddress}:{port}
          </p>
        )}

        {/* PUB-43. The most consequential thing this panel does, so it is a
            deliberate act rather than a default. */}
        <label className="flex items-start gap-2 rounded-md border border-warning/40 bg-warning/5 p-3 text-sm">
          <input
            type="checkbox"
            checked={acknowledged}
            onChange={(e) => setAcknowledged(e.target.checked)}
            className="mt-0.5"
          />
          <span>
            I understand this makes the service reachable by <strong>anyone on the internet</strong>
            . Cloudflare Access is not configured by this portal; if the service has no
            authentication of its own, it will have none.
          </span>
        </label>

        {error && (
          <p role="alert" className="text-sm text-danger">
            {error}
          </p>
        )}

        <div className="flex justify-end gap-2">
          <button
            onClick={onClose}
            className="rounded-md border border-border px-3 py-2 text-sm hover:bg-surface"
          >
            Cancel
          </button>
          <button
            onClick={() => publish.mutate()}
            disabled={!ready || !acknowledged || publish.isPending}
            className="rounded-md bg-accent px-3 py-2 text-sm font-medium text-white disabled:opacity-40"
          >
            {publish.isPending ? 'Publishing…' : 'Publish'}
          </button>
        </div>
      </div>
    </div>
  )
}

function ModeButton({
  active,
  onClick,
  children,
}: {
  active: boolean
  onClick: () => void
  children: React.ReactNode
}) {
  return (
    <button
      onClick={onClick}
      aria-pressed={active}
      className={`rounded-md px-3 py-1.5 text-sm ${
        active
          ? 'border border-accent bg-accent/10 text-accent'
          : 'border border-border hover:bg-surface'
      }`}
    >
      {children}
    </button>
  )
}

// The causes an administrator can act on are named. The distinction that runs
// through this whole feature: an error about Cloudflare means Cloudflare
// refused and nothing in the portal will change it.
function publishError(err: unknown): string {
  if (!(err instanceof ApiError)) return 'The app could not be published.'
  switch (err.code) {
    case 'publish.exposure_not_acknowledged':
      return 'Confirm the exposure checkbox before publishing.'
    case 'publish.zone_not_allowed':
      return err.detail
    case 'publish.stale':
      return 'The routing table changed while this dialog was open. Close it, reload, and try again.'
    case 'publish.would_break_portal':
      return err.detail
    case 'publish.write_not_permitted':
      return err.detail
    case 'conflict':
      return err.detail
    default:
      return err.detail || 'The app could not be published.'
  }
}
