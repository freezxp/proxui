import { useEffect, useRef } from 'react'
import { useQuery } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { ContainerDeployment } from '@/api/types'
import { Drawer } from '@/components/Drawer'
import { absoluteTime, relativeTime } from '@/lib/format'

/** What the install script printed (ADR 0012).
 *
 *  This is the first thing in the portal to show the output of a
 *  non-interactive command run on a node. Everything before it reported a state
 *  and a sentence, which is enough for a step that either worked or did not,
 *  and not enough for a fifteen-minute script that stopped two thirds of the
 *  way through.
 *
 *  The transcript lives on the node, not here, so it survives a portal restart
 *  and this only has to fetch it.
 */
export function DeploymentLog({ id, onClose }: { id: string; onClose: () => void }) {
  const view = useRef<HTMLPreElement>(null)

  const deployment = useQuery({
    queryKey: ['container-deployment', id],
    queryFn: () => api.get<ContainerDeployment>(`/container-deployments/${id}`),
    refetchInterval: (q) => {
      const state = q.state.data?.state
      return state === 'ready' || state === 'failed' ? false : 5000
    },
  })

  const data = deployment.data
  const running = data?.state === 'pending' || data?.state === 'deploying'

  // Follow the output while it is still arriving, and stop once it has stopped:
  // scrolling somebody away from the line they were reading is worse than not
  // following at all.
  useEffect(() => {
    if (running && view.current) {
      view.current.scrollTop = view.current.scrollHeight
    }
  }, [data?.log, running])

  return (
    <Drawer title={data ? `${data.app_name} on ${data.node}` : 'Deployment'} onClose={onClose}>
      {!data ? (
        <p className="text-sm text-muted">Loading…</p>
      ) : (
        <div className="space-y-3 text-sm">
          <dl className="grid grid-cols-2 gap-y-1 text-xs">
            <dt className="text-muted">State</dt>
            <dd className={data.state === 'failed' ? 'text-danger' : ''}>
              {data.state === 'deploying' ? 'installing…' : data.state}
              {data.exit_code !== undefined && data.exit_code !== 0 && ` (exit ${data.exit_code})`}
            </dd>
            {data.ctid && (
              <>
                <dt className="text-muted">Container</dt>
                <dd className="font-mono">{data.ctid}</dd>
              </>
            )}
            <dt className="text-muted">Started</dt>
            <dd title={absoluteTime(data.created_at)}>{relativeTime(data.created_at)}</dd>
            {data.requested_by && (
              <>
                <dt className="text-muted">Asked for by</dt>
                <dd>{data.requested_by}</dd>
              </>
            )}
          </dl>

          {data.error && (
            <p className="rounded-md bg-danger/10 p-2 text-xs text-danger">{data.error}</p>
          )}

          {running && (
            <p className="text-xs text-muted">
              Still installing. This takes a few minutes, and it keeps going on the node whether or
              not this page is open.
            </p>
          )}

          <pre
            ref={view}
            className="max-h-[28rem] overflow-auto whitespace-pre-wrap break-words rounded-md bg-surface-inset p-2 font-mono text-[10px] leading-relaxed"
          >
            {data.log?.trim() ? data.log : 'Nothing has been printed yet.'}
          </pre>

          {data.state === 'ready' && (
            <p className="text-xs text-muted">
              The container appears in the inventory at the next synchronization, with its console
              and terminal already working. The portal does not manage it beyond this record.
            </p>
          )}
        </div>
      )}
    </Drawer>
  )
}
