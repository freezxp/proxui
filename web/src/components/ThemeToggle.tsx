import { useEffect, useState } from 'react'
import { applyTheme, storedTheme, watchSystemTheme, type Theme } from '@/lib/theme'

const OPTIONS: { value: Theme; label: string }[] = [
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
  { value: 'system', label: 'System' },
]

export function ThemeToggle() {
  const [theme, setTheme] = useState<Theme>(storedTheme)

  useEffect(() => {
    applyTheme(theme)
    return watchSystemTheme()
  }, [theme])

  return (
    <label className="flex items-center gap-2 text-sm">
      <span className="sr-only">Theme</span>
      <select
        value={theme}
        onChange={(e) => setTheme(e.target.value as Theme)}
        className="rounded-md border border-border bg-surface-raised px-2 py-1 text-sm outline-none focus:border-accent"
      >
        {OPTIONS.map((option) => (
          <option key={option.value} value={option.value}>
            {option.label}
          </option>
        ))}
      </select>
    </label>
  )
}
