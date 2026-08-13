export type Theme = 'light' | 'dark' | 'system'

const STORAGE_KEY = 'proxui.theme'

/** Reads the stored preference, defaulting to following the operating system. */
export function storedTheme(): Theme {
  const raw = localStorage.getItem(STORAGE_KEY)
  return raw === 'light' || raw === 'dark' || raw === 'system' ? raw : 'system'
}

/**
 * Applies a theme to the document.
 *
 * "system" deliberately keeps following the OS afterwards rather than
 * snapshotting it, so a machine that switches at sunset takes the portal with
 * it without a reload.
 */
export function applyTheme(theme: Theme): void {
  localStorage.setItem(STORAGE_KEY, theme)
  const prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches
  const dark = theme === 'dark' || (theme === 'system' && prefersDark)
  document.documentElement.classList.toggle('dark', dark)
}

/** Starts following OS changes while the preference is "system". */
export function watchSystemTheme(): () => void {
  const query = window.matchMedia('(prefers-color-scheme: dark)')
  const handler = () => {
    if (storedTheme() === 'system') applyTheme('system')
  }
  query.addEventListener('change', handler)
  return () => query.removeEventListener('change', handler)
}
