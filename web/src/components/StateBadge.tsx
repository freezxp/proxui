import type { VMState } from '@/api/types'

const STYLES: Record<VMState, string> = {
  running: 'bg-running/15 text-running',
  stopped: 'bg-stopped/15 text-stopped',
  paused: 'bg-paused/15 text-paused',
  suspended: 'bg-paused/15 text-paused',
  unknown: 'bg-stopped/15 text-stopped',
}

export function StateBadge({ state, stale }: { state: VMState; stale?: boolean }) {
  return (
    <span className="inline-flex items-center gap-1.5">
      <span className={`rounded-full px-2 py-0.5 text-xs font-medium capitalize ${STYLES[state]}`}>
        {state}
      </span>
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
