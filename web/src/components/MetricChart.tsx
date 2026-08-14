import { useEffect, useRef, useState } from 'react'
import uPlot from 'uplot'
import 'uplot/dist/uPlot.min.css'

export interface Series {
  label: string
  /** CSS colour; read from theme tokens so charts follow light and dark. */
  color: string
  values: (number | null)[]
  /** Formats a value for the axis and tooltip. */
  format: (value: number) => string
}

interface Props {
  timestamps: number[]
  series: Series[]
  height?: number
  /** Fixes the y-axis to 0-100 for percentages, so a quiet VM does not look
   *  busy because the axis auto-scaled to its 2% range. */
  percentScale?: boolean
}

function cssVar(name: string): string {
  const value = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return value ? `rgb(${value})` : '#888'
}

/**
 * A time-series chart.
 *
 * uPlot owns a canvas imperatively, so this component's job is lifecycle:
 * create on mount, feed new data without recreating, and rebuild when the
 * theme changes because the colours are baked into the options (see
 * docs/adr/0001).
 */
export function MetricChart({ timestamps, series, height = 200, percentScale }: Props) {
  const container = useRef<HTMLDivElement>(null)
  const chart = useRef<uPlot | null>(null)
  const [width, setWidth] = useState(0)
  const [themeTick, setThemeTick] = useState(0)

  // Rebuild on theme change: uPlot bakes colours into its options at
  // construction, so a live toggle needs a fresh instance.
  useEffect(() => {
    const observer = new MutationObserver(() => setThemeTick((n) => n + 1))
    observer.observe(document.documentElement, { attributes: true, attributeFilter: ['class'] })
    return () => observer.disconnect()
  }, [])

  useEffect(() => {
    if (!container.current) return
    const observer = new ResizeObserver((entries) => {
      setWidth(Math.floor(entries[0].contentRect.width))
    })
    observer.observe(container.current)
    return () => observer.disconnect()
  }, [])

  useEffect(() => {
    if (!container.current || width === 0) return

    const axisColor = cssVar('--muted')
    const gridColor = cssVar('--border')

    const options: uPlot.Options = {
      width,
      height,
      cursor: { drag: { x: true, y: false } },
      legend: { live: true },
      scales: { y: percentScale ? { range: [0, 100] } : {} },
      axes: [
        { stroke: axisColor, grid: { stroke: gridColor, width: 1 }, ticks: { stroke: gridColor } },
        {
          stroke: axisColor,
          grid: { stroke: gridColor, width: 1 },
          ticks: { stroke: gridColor },
          values: (_u, splits) => splits.map((v) => series[0]?.format(v) ?? String(v)),
        },
      ],
      series: [
        {},
        ...series.map((s) => ({
          label: s.label,
          stroke: s.color,
          width: 1.5,
          fill: `${s.color}1a`,
          value: (_u: uPlot, v: number | null) => (v == null ? '—' : s.format(v)),
        })),
      ],
    }

    const data: uPlot.AlignedData = [
      timestamps,
      ...series.map((s) => s.values),
    ] as uPlot.AlignedData

    chart.current?.destroy()
    chart.current = new uPlot(options, data, container.current)

    return () => {
      chart.current?.destroy()
      chart.current = null
    }
    // series identity changes every render; the data array below covers updates
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [width, height, percentScale, themeTick, timestamps, series])

  if (timestamps.length === 0) {
    return (
      <div
        ref={container}
        style={{ height }}
        className="flex items-center justify-center rounded-md border border-dashed border-border text-sm text-muted"
      >
        No samples in this range
      </div>
    )
  }

  return <div ref={container} className="w-full" />
}
