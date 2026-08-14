# ADR 0001 — uPlot instead of ECharts for metric charts

**Status:** accepted · **Date:** 2026-08-14 · **Supersedes:** the charting choice in [docs/05-system-architecture.md §5.4](../05-system-architecture.md) and [docs/11-project-structure.md §11.3](../11-project-structure.md)

## Context

The design package names ECharts as the charting library. Sprint 13 is the first sprint that actually draws a chart, so this is the point where the choice becomes real.

What the portal needs to draw is narrow: line charts of time-series metrics, up to five series, between 60 and ~2,900 points depending on the range the resolution selector picks ([docs/03-frs.md](../03-frs.md) PERF-02). No pies, maps, treemaps, gauges, or the rest of ECharts' catalogue.

Two design constraints bear on this directly:

- **NFR-P5** caps the initial bundle at 1 MB gzipped, with charts lazy-loaded.
- **NFR-P2** wants chart queries answered in under 800 ms; rendering has to keep pace or the budget is spent in the browser instead.

## Decision

Use **uPlot** with a small in-house React wrapper, rather than ECharts.

## Rationale

| | uPlot | ECharts |
|---|---|---|
| Size (gzipped) | ~15 KB | ~150 KB tree-shaken, 400 KB+ full |
| Built for | time series specifically | every chart type |
| 2,900-point redraw | sub-millisecond | tens of milliseconds |
| React integration | none; needs a ~60-line wrapper | community wrappers |
| Styling | plain CSS/canvas, theme via options | rich theming built in |

The deciding factor is fit rather than size alone. uPlot exists to draw exactly the chart this product needs, and it does so an order of magnitude smaller and faster. ECharts' breadth is its value, and this product uses none of it.

The wrapper is the honest cost: uPlot has no React binding, so we own ~60 lines that manage the instance lifecycle and feed it theme colours. That is a small, comprehensible amount of code, and it is the same code that would otherwise be someone else's dependency.

## Consequences

- The portal owns a small chart wrapper (`web/src/components/MetricChart.tsx`), including redrawing on theme change, which a batteries-included library would have handled.
- Anything beyond line and area charts — should a capacity-planning view later want a heatmap — needs either a second library or hand-drawn SVG. Given the product's charts are all time series, that is unlikely to bite.
- Charts stay lazy-loaded regardless, so the initial bundle is unaffected either way; the win is in interaction cost and in not shipping an unused chart catalogue.

## Alternatives considered

- **ECharts as designed.** Rejected: ten times the size for capabilities the product does not use.
- **Recharts.** More idiomatic in React, but SVG-based rendering degrades on multi-thousand-point series, which is precisely the year-range case the metrics pipeline was built for.
- **Hand-drawn SVG.** Viable for a sparkline, not for axes, tooltips, and zoom on five synchronized series.
