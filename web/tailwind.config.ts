import type { Config } from 'tailwindcss'

export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  // Class strategy rather than media: the portal remembers a chosen theme,
  // and an operator on a bright show floor should not be forced into dark
  // mode by their OS setting.
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        surface: 'rgb(var(--surface) / <alpha-value>)',
        'surface-raised': 'rgb(var(--surface-raised) / <alpha-value>)',
        border: 'rgb(var(--border) / <alpha-value>)',
        content: 'rgb(var(--content) / <alpha-value>)',
        muted: 'rgb(var(--muted) / <alpha-value>)',
        accent: 'rgb(var(--accent) / <alpha-value>)',
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
