/**
 * Clipboard access that works where the portal actually runs.
 *
 * `navigator.clipboard` exists only in a secure context. A portal reached over
 * HTTPS has it; the same portal reached at http://192.168.1.10:8080 — the
 * documented LAN deployment — does not, and neither does an IP address behind
 * a plain-HTTP reverse proxy. Both are supported deployments, so every
 * clipboard path here degrades instead of failing.
 *
 * `readText` degrades to nothing at all: no fallback can read a clipboard the
 * browser will not hand over, which is why the terminal keeps a paste panel
 * for the case where this returns null.
 */

/** Copies text, reporting whether it worked. */
export async function copyText(text: string): Promise<boolean> {
  if (text === '') return false

  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text)
      return true
    }
  } catch {
    /* denied, or not a secure context — fall through */
  }

  // execCommand('copy') is deprecated and still the only thing that works on
  // a plain-HTTP origin. It needs a real, focusable, on-screen element:
  // display:none or visibility:hidden makes the selection empty and the copy
  // silently do nothing, so the textarea is merely moved out of view.
  try {
    const scratch = document.createElement('textarea')
    scratch.value = text
    scratch.setAttribute('readonly', '')
    scratch.style.position = 'fixed'
    scratch.style.top = '-1000px'
    scratch.style.opacity = '0'
    document.body.appendChild(scratch)
    scratch.select()
    const ok = document.execCommand('copy')
    document.body.removeChild(scratch)
    return ok
  } catch {
    return false
  }
}

/** Reads the clipboard, or null where the browser will not allow it. */
export async function readText(): Promise<string | null> {
  try {
    if (navigator.clipboard?.readText) return await navigator.clipboard.readText()
  } catch {
    /* the permission prompt was refused, or this is not a secure context */
  }
  return null
}

/** Whether reading is even worth attempting, for deciding what a button says. */
export function canReadClipboard(): boolean {
  return typeof navigator.clipboard?.readText === 'function'
}
