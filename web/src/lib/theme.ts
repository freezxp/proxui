/**
 * Appearance has two independent axes, and conflating them is what makes a
 * settings screen confusing.
 *
 *   Mode  — light, dark, or whatever the machine says.
 *   Theme — which palette and typeface, in either mode.
 *
 * A theme defines every token twice, once per mode, so choosing "Slate" does
 * not decide light or dark and choosing dark does not decide the palette.
 */

export type Mode = 'light' | 'dark' | 'system'

/** The palettes, in the order they are offered. */
export type Theme = 'slate' | 'classic'

const MODE_KEY = 'proxui.theme'
const THEME_KEY = 'proxui.palette'

/** The attribute the stylesheet keys its second palette off. */
const THEME_ATTR = 'data-theme'

/**
 * Reads the stored mode, defaulting to following the operating system.
 *
 * Storage is wrapped because it throws outright in some private-browsing
 * modes, and an appearance preference is never worth a blank page.
 */
export function storedMode(): Mode {
  const raw = read(MODE_KEY)
  return raw === 'light' || raw === 'dark' || raw === 'system' ? raw : 'system'
}

/** Reads the stored theme. Slate is the default, and the one dnsprox shares. */
export function storedTheme(): Theme {
  return read(THEME_KEY) === 'classic' ? 'classic' : 'slate'
}

/**
 * Applies a mode to the document.
 *
 * "system" deliberately keeps following the OS afterwards rather than
 * snapshotting it, so a machine that switches at sunset takes the portal with
 * it without a reload.
 */
export function applyMode(mode: Mode): void {
  write(MODE_KEY, mode)
  const prefersDark = window.matchMedia('(prefers-color-scheme: dark)').matches
  const dark = mode === 'dark' || (mode === 'system' && prefersDark)
  document.documentElement.classList.toggle('dark', dark)
}

/**
 * Applies a theme to the document.
 *
 * The default palette lives on `:root` and needs no attribute; the attribute is
 * written for it anyway, because "which one am I looking at" should be
 * answerable from the element inspector rather than by elimination.
 */
export function applyTheme(theme: Theme): void {
  write(THEME_KEY, theme)
  document.documentElement.setAttribute(THEME_ATTR, theme)
}

/** Starts following OS changes while the mode is "system". */
export function watchSystemMode(): () => void {
  const query = window.matchMedia('(prefers-color-scheme: dark)')
  const handler = () => {
    if (storedMode() === 'system') applyMode('system')
  }
  query.addEventListener('change', handler)
  return () => query.removeEventListener('change', handler)
}

function read(key: string): string | null {
  try {
    return window.localStorage.getItem(key)
  } catch {
    return null
  }
}

function write(key: string, value: string): void {
  try {
    window.localStorage.setItem(key, value)
  } catch {
    // Not being able to remember the choice is not a reason to refuse it.
  }
}
