import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { api, detailOf } from '@/api/client'
import { relativeTime } from '@/lib/format'

/**
 * What this platform's nodes need, and installing what the portal can
 * (ADR 0011).
 *
 * Three of the portal's features run on a node rather than against the API,
 * because Proxmox has no API for what they do. None of them fails loudly when
 * the node is missing what it needs — a temperature chart is simply empty, or a
 * guest arrives with no agent and therefore no address — so before this section
 * existed each was discovered weeks later by somebody noticing an absence.
 *
 * Checking is a button, not a page load: it costs an SSH handshake per node for
 * an answer that changes about once a year.
 */

type InstallState = {
  node: string
  prerequisite: string
  state: 'running' | 'installed' | 'failed'
  error?: string
  started_at: string
  finished_at?: string
}

type Prerequisite = {
  id: string
  name: string
  needed: string
  present: boolean
  installable: boolean
  packages?: string[]
  command?: string
  install?: InstallState
}

type NodeReadiness = {
  node: string
  address: string
  reachable: boolean
  problem?: string
  fingerprint?: string
  prerequisites: Prerequisite[]
}

type Privileges = {
  missing: string[]
  provisioning_available: boolean
  missing_provisioning: string[]
  template_build_available: boolean
  missing_template: string[]
  warnings?: string[]
}

type Readiness = {
  portal_key: boolean
  nodes: NodeReadiness[]
  privileges: Privileges
}

export function ReadinessSection({ platformID }: { platformID: string }) {
  const [asked, setAsked] = useState(false)
  const queryClient = useQueryClient()

  const readiness = useQuery({
    queryKey: ['readiness', platformID],
    queryFn: () => api.get<Readiness>(`/platforms/${platformID}/readiness`),
    enabled: asked,
    // An installation is answered the moment it starts, because apt-get takes
    // minutes. While one is in flight the report is the only place its outcome
    // will appear, so ask again until it has one.
    refetchInterval: (query) =>
      query.state.data?.nodes.some((n) =>
        n.prerequisites.some((p) => p.install?.state === 'running'),
      )
        ? 5000
        : false,
  })

  const data = readiness.data

  return (
    <div className="space-y-2 rounded-md border border-border p-3">
      <div className="flex items-start justify-between gap-3">
        <div>
          <p className="text-sm font-medium">Readiness</p>
          <p className="text-xs text-muted">
            What this cluster&apos;s nodes need for temperatures and template building, and what
            this platform&apos;s token is allowed to do.
          </p>
        </div>
        <button
          onClick={() => {
            setAsked(true)
            if (asked) void readiness.refetch()
          }}
          disabled={readiness.isFetching}
          className="shrink-0 rounded-md border border-border px-2 py-1 text-xs disabled:opacity-40"
        >
          {readiness.isFetching ? 'Checking…' : asked ? 'Check again' : 'Check'}
        </button>
      </div>

      {readiness.isError && (
        <p className="text-xs text-danger">
          {detailOf(readiness.error, 'The nodes could not be checked.')}
        </p>
      )}

      {data && (
        <div className="space-y-3 pt-1">
          <PrivilegeReport privileges={data.privileges} />

          {!data.portal_key && (
            <p className="rounded-md bg-paused/10 p-2 text-xs text-paused">
              The portal has no SSH key of its own, so it cannot reach any node. Generate one in{' '}
              <span className="font-medium">Settings → SSH key</span>.
            </p>
          )}

          {data.nodes.length === 0 ? (
            <p className="text-xs text-muted">
              This platform reports no nodes the portal needs anything on.
            </p>
          ) : (
            data.nodes.map((node) => (
              <NodeCard
                key={node.node}
                platformID={platformID}
                node={node}
                onInstalled={() =>
                  void queryClient.invalidateQueries({ queryKey: ['readiness', platformID] })
                }
              />
            ))
          )}
        </div>
      )}
    </div>
  )
}

/** The credential's half of the same question: what this token may do. */
function PrivilegeReport({ privileges }: { privileges: Privileges }) {
  return (
    <div className="space-y-1 rounded-md bg-surface-raised p-2 text-xs">
      <p className="font-medium">Platform token</p>
      {privileges.missing.length > 0 && (
        <p className="text-danger">Missing for basic operation: {privileges.missing.join(', ')}</p>
      )}
      <Capability
        label="Create and destroy guests"
        available={privileges.provisioning_available}
        missing={privileges.missing_provisioning}
      />
      <Capability
        label="Build templates"
        available={privileges.template_build_available}
        missing={privileges.missing_template}
      />
      {privileges.warnings?.map((w) => (
        <p key={w} className="text-muted">
          {w}
        </p>
      ))}
      {(!privileges.provisioning_available || !privileges.template_build_available) && (
        // Deliberately an instruction rather than a button: widening a token is
        // a decision made on the cluster, and a portal that could widen its own
        // credential would have no limits worth reporting (ADR 0010).
        <p className="text-muted">
          Grant these on the cluster, in Datacenter → Permissions, to the token this platform uses.
        </p>
      )}
    </div>
  )
}

function Capability({
  label,
  available,
  missing,
}: {
  label: string
  available: boolean
  missing: string[]
}) {
  return (
    <p className={available ? 'text-running' : 'text-muted'}>
      {available ? '✓' : '✗'} {label}
      {!available && missing.length > 0 && (
        <span className="block pl-4 font-mono text-[11px] text-muted">{missing.join(', ')}</span>
      )}
    </p>
  )
}

function NodeCard({
  platformID,
  node,
  onInstalled,
}: {
  platformID: string
  node: NodeReadiness
  onInstalled: () => void
}) {
  return (
    <div className="rounded-md border border-border">
      <div className="flex items-baseline justify-between gap-2 border-b border-border px-2 py-1.5">
        <span className="text-xs font-medium">{node.node}</span>
        <span className="font-mono text-[11px] text-muted" title={node.fingerprint}>
          {node.address}
        </span>
      </div>

      {!node.reachable ? (
        // Unknown is not the same as missing: a node that would not let the
        // portal in has said nothing about what it has installed, and listing
        // its prerequisites as absent would send somebody to fix the wrong
        // thing.
        <p className="px-2 py-2 text-xs text-paused">
          {node.problem || 'The node did not answer.'}
        </p>
      ) : (
        <ul className="divide-y divide-border">
          {node.prerequisites.map((p) => (
            <li key={p.id} className="px-2 py-2">
              <PrerequisiteRow
                platformID={platformID}
                node={node.node}
                prerequisite={p}
                onInstalled={onInstalled}
              />
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

function PrerequisiteRow({
  platformID,
  node,
  prerequisite,
  onInstalled,
}: {
  platformID: string
  node: string
  prerequisite: Prerequisite
  onInstalled: () => void
}) {
  const [confirming, setConfirming] = useState(false)
  const [error, setError] = useState('')
  const running = prerequisite.install?.state === 'running'

  const install = useMutation({
    mutationFn: () =>
      api.post(`/platforms/${platformID}/nodes/${encodeURIComponent(node)}/install`, {
        prerequisite: prerequisite.id,
      }),
    onSuccess: () => {
      setConfirming(false)
      onInstalled()
    },
    onError: (err) => setError(detailOf(err, 'The installation could not be started.')),
  })

  return (
    <div className="space-y-1.5">
      <div className="flex items-start justify-between gap-2">
        <div className="min-w-0">
          <p className={`text-xs ${prerequisite.present ? 'text-running' : 'text-paused'}`}>
            {prerequisite.present ? '✓' : '✗'} {prerequisite.name}
          </p>
          <p className="text-[11px] text-muted">{prerequisite.needed}</p>
        </div>
        {!prerequisite.present && prerequisite.installable && !confirming && (
          <button
            onClick={() => {
              setError('')
              setConfirming(true)
            }}
            disabled={running}
            className="shrink-0 rounded-md border border-border px-2 py-1 text-[11px] disabled:opacity-40"
          >
            {running ? 'Installing…' : 'Install'}
          </button>
        )}
      </div>

      {confirming && (
        // The command is shown rather than summarised. The portal is asking to
        // run this as root on a hypervisor, and the honest way to ask is to say
        // exactly what it is.
        <div className="space-y-1.5 rounded-md bg-surface-raised p-2">
          <p className="text-[11px] text-muted">
            This runs on <span className="font-medium">{node}</span> as root:
          </p>
          <code className="block break-all rounded bg-surface p-1.5 font-mono text-[10px]">
            {prerequisite.command}
          </code>
          <div className="flex gap-2">
            <button
              onClick={() => install.mutate()}
              disabled={install.isPending}
              className="rounded-md bg-accent px-2 py-1 text-[11px] text-white disabled:opacity-40"
            >
              {install.isPending ? 'Starting…' : `Install on ${node}`}
            </button>
            <button
              onClick={() => setConfirming(false)}
              className="rounded-md border border-border px-2 py-1 text-[11px]"
            >
              Cancel
            </button>
          </div>
        </div>
      )}

      {error && <p className="text-[11px] text-danger">{error}</p>}

      {prerequisite.install && (
        <p
          className={`text-[11px] ${
            prerequisite.install.state === 'failed'
              ? 'text-danger'
              : prerequisite.install.state === 'running'
                ? 'text-muted'
                : 'text-running'
          }`}
        >
          {prerequisite.install.state === 'running'
            ? `Installing since ${relativeTime(prerequisite.install.started_at)} — this takes a few minutes`
            : prerequisite.install.state === 'failed'
              ? `Installation failed: ${prerequisite.install.error}`
              : `Installed ${relativeTime(prerequisite.install.finished_at ?? prerequisite.install.started_at)}`}
        </p>
      )}
    </div>
  )
}
