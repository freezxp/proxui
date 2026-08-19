import { useQuery } from '@tanstack/react-query'
import { api } from '@/api/client'
import type { NodeSensors, SensorReading, SensorSummary } from '@/api/types'

/** How much of the chip's own critical point is left, 0 (at the limit) to 1.
 *
 *  This is the comparison worth colouring by. A VRM at 75°C with a 125°C limit
 *  is idling and a CPU at 75°C with an 84°C limit is not, so colouring by
 *  degrees would paint the wrong one red. */
export function headroom(reading: SensorReading): number | null {
  if (!reading.crit || reading.crit <= 0) return null
  return Math.max(0, (reading.crit - reading.value) / reading.crit)
}

/** Sensors with no declared limit fall back to degrees, which is all anybody
 *  can say about them. The thresholds are deliberately generous: this is the
 *  fallback for hardware nobody could ask. */
function toneFor(reading: SensorReading): string {
  const left = headroom(reading)
  if (left === null) {
    if (reading.value >= 85) return 'text-danger'
    if (reading.value >= 70) return 'text-warning'
    return 'text-muted'
  }
  if (left <= 0.1) return 'text-danger'
  if (left <= 0.2) return 'text-warning'
  return 'text-muted'
}

export function formatTemp(value: number): string {
  return `${Math.round(value)}°C`
}

/** The chip name without the bus suffix `sensors` appends, so a narrow column
 *  reads "coretemp" rather than "coretemp-isa-0000". */
export function shortChip(chip: string): string {
  for (const sep of ['-isa-', '-pci-', '-virtual-', '-acpi-', '-i2c-']) {
    const i = chip.indexOf(sep)
    if (i > 0) return chip.slice(0, i)
  }
  return chip
}

/** The hottest reading, for a table cell. A node that has never answered shows
 *  a dash rather than a zero: no reading and a cold node are different things. */
export function TempCell({ summary }: { summary?: SensorSummary }) {
  if (!summary || summary.count === 0) {
    return <span className="text-muted">—</span>
  }
  const reading = summary.hottest
  return (
    <span
      className={toneFor(reading)}
      title={`${reading.chip} ${reading.label}${reading.crit ? ` · critical at ${formatTemp(reading.crit)}` : ''}`}
    >
      {formatTemp(reading.value)}
      <span className="ml-1 text-xs text-muted">{reading.label}</span>
    </span>
  )
}

/** Every sensor on one node, plus why there are none when there are none.
 *
 *  The empty case is the common one and carries the whole setup instruction:
 *  most nodes have neither the portal's key installed nor lm-sensors, and
 *  saying "no readings" without saying what to do about it helps nobody. */
export function NodeSensorPanel({ hostId, hostName }: { hostId: string; hostName: string }) {
  const sensors = useQuery({
    queryKey: ['host-sensors', hostId],
    queryFn: () => api.get<NodeSensors>(`/hosts/${hostId}/sensors`),
    refetchInterval: 60_000,
  })

  if (sensors.isLoading) {
    return <div className="px-4 py-3 text-sm text-muted">Reading {hostName}…</div>
  }

  const data = sensors.data
  const readings = data?.readings ?? []

  if (readings.length === 0) {
    return (
      <div className="px-4 py-3 text-sm">
        <div className="text-muted">
          {data?.node?.last_error ?? 'This node has not been read yet.'}
        </div>
        <p className="mt-2 max-w-2xl text-xs text-muted">
          Proxmox publishes no temperature, so the portal reads it from the node itself. Install the
          portal&apos;s public key from <strong>Settings → SSH key</strong> into{' '}
          <code>/root/.ssh/authorized_keys</code> on {hostName}, and{' '}
          <code>apt install lm-sensors</code> there.
        </p>
      </div>
    )
  }

  const byChip = new Map<string, typeof readings>()
  for (const reading of readings) {
    const group = byChip.get(reading.chip) ?? []
    group.push(reading)
    byChip.set(reading.chip, group)
  }

  return (
    <div className="px-4 py-3">
      <div className="flex flex-wrap gap-x-8 gap-y-4">
        {[...byChip.entries()].map(([chip, group]) => (
          <div key={chip}>
            <div className="mb-1 text-xs font-medium uppercase tracking-wide text-muted">
              {shortChip(chip)}
            </div>
            <table className="text-sm">
              <tbody>
                {group.map((reading) => (
                  <tr key={`${reading.chip}-${reading.label}`}>
                    <td className="pr-4 text-muted">{reading.label}</td>
                    <td className={`pr-3 text-right tabular-nums ${toneFor(reading)}`}>
                      {reading.kind === 'fan_rpm'
                        ? `${Math.round(reading.value)} rpm`
                        : formatTemp(reading.value)}
                    </td>
                    <td className="text-xs text-muted">
                      {reading.crit ? `limit ${formatTemp(reading.crit)}` : ''}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ))}
      </div>

      {data?.node && (
        <div className="mt-3 border-t border-border pt-2 text-xs text-muted">
          {data.node.ssh_user}@{data.node.address} · {data.node.algorithm}{' '}
          <code>{data.node.fingerprint}</code>
          {data.node.last_error && (
            <span className="ml-2 text-warning">{data.node.last_error}</span>
          )}
        </div>
      )}
    </div>
  )
}
