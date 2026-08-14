/** Formatters shared across the UI, so a byte count or an uptime reads the
 *  same way on every page. */

const UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB']

export function bytes(value: number): string {
  if (!value) return '—'
  let size = value
  let unit = 0
  while (size >= 1024 && unit < UNITS.length - 1) {
    size /= 1024
    unit++
  }
  // Whole numbers below 10 look odd with a decimal ("8.0 GiB"), and above it
  // the decimal is noise.
  return `${size < 10 ? size.toFixed(1) : Math.round(size)} ${UNITS[unit]}`
}

export function uptime(seconds: number): string {
  if (!seconds) return '—'
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${minutes}m`
  return `${minutes}m`
}

export function percent(value: number): string {
  return `${value.toFixed(1)}%`
}

/** Relative time for table cells; the absolute value goes in a title attribute
 *  so hovering still answers "exactly when?". */
export function relativeTime(iso: string): string {
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return '—'
  const seconds = Math.round((Date.now() - then) / 1000)
  if (seconds < 60) return 'just now'
  if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`
  if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`
  return `${Math.floor(seconds / 86400)}d ago`
}

export function absoluteTime(iso: string): string {
  const date = new Date(iso)
  return Number.isNaN(date.getTime()) ? '' : date.toLocaleString()
}
