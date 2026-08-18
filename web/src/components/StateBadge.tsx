import type { VMState } from '@/api/types'

const STYLES: Record<VMState, string> = {
  running: 'bg-running/15 text-running',
  stopped: 'bg-stopped/15 text-stopped',
  paused: 'bg-paused/15 text-paused',
  suspended: 'bg-paused/15 text-paused',
  unknown: 'bg-stopped/15 text-stopped',
}

export function StateBadge({
  state,
  stale,
  liveAt,
}: {
  state: VMState
  stale?: boolean
  /** When the platform itself last confirmed this state (docs/10 §10.6).
   *  Absent means it is what the last sync found, which is worth saying: the
   *  difference decides whether a power button can be trusted. */
  liveAt?: string
}) {
  const confirmed = liveAt
    ? `Confirmed by the platform at ${new Date(liveAt).toLocaleTimeString()}`
    : ''
  return (
    <span className="inline-flex items-center gap-1.5">
      <span
        className={`rounded-full px-2 py-0.5 text-xs font-medium capitalize ${STYLES[state]}`}
        title={confirmed || 'From the last synchronization, not confirmed just now'}
      >
        {state}
      </span>
      {liveAt && (
        <span
          aria-label="Confirmed by the platform just now"
          title={confirmed}
          className="h-1.5 w-1.5 rounded-full bg-state-running"
        />
      )}
      {/* Stale data is always labelled rather than quietly shown as current:
          an operator acting on a stopped VM that is actually running is the
          failure this prevents. */}
      {stale && (
        <span className="rounded-full bg-stale/15 px-2 py-0.5 text-xs font-medium text-stale">
          stale
        </span>
      )}
    </span>
  )
}
