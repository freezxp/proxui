import type { Config } from 'tailwindcss'

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  // Class strategy rather than media: the portal remembers a chosen theme,
  // and an operator on a bright show floor should not be forced into dark
  // mode by their OS setting.
  darkMode: 'class',
  theme: {
    extend: {
      fontFamily: {
        // IBM Plex, self-hosted through @fontsource rather than linked from a
        // font CDN: this is a portal somebody runs on their own network, and a
        // stylesheet fetched from Google on every load would both leak who is
        // using it and leave an air-gapped install rendering in a fallback.
        sans: ['IBM Plex Sans', 'ui-sans-serif', 'system-ui', 'sans-serif'],
        mono: ['IBM Plex Mono', 'ui-monospace', 'SFMono-Regular', 'Menlo', 'monospace'],
      },
      colors: {
        surface: 'rgb(var(--surface) / <alpha-value>)',
        'surface-raised': 'rgb(var(--surface-raised) / <alpha-value>)',
        'surface-inset': 'rgb(var(--surface-inset) / <alpha-value>)',
        border: 'rgb(var(--border) / <alpha-value>)',
        'border-strong': 'rgb(var(--border-strong) / <alpha-value>)',
        content: 'rgb(var(--content) / <alpha-value>)',
        'content-soft': 'rgb(var(--content-soft) / <alpha-value>)',
        muted: 'rgb(var(--muted) / <alpha-value>)',
        accent: 'rgb(var(--accent) / <alpha-value>)',
        'accent-strong': 'rgb(var(--accent-strong) / <alpha-value>)',
        'accent-wash': 'rgb(var(--accent-wash) / <alpha-value>)',
        // VM state colours are semantic, not decorative: the same green
        // means running everywhere in the product.
        running: 'rgb(var(--state-running) / <alpha-value>)',
        stopped: 'rgb(var(--state-stopped) / <alpha-value>)',
        paused: 'rgb(var(--state-paused) / <alpha-value>)',
        stale: 'rgb(var(--state-stale) / <alpha-value>)',
        warning: 'rgb(var(--warning) / <alpha-value>)',
        danger: 'rgb(var(--danger) / <alpha-value>)',
      },
    },
  },
  plugins: [],
} satisfies Config
