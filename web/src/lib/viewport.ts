import { useEffect, useState } from 'react'

/**
 * The height of the part of the viewport a phone is actually showing.
 *
 * A full-screen page — console, SSH terminal — sizes itself to the viewport
 * and puts its key bar at the bottom. Both viewport units get that wrong on a
 * phone, in two different ways:
 *
 * - `100vh` is the height with the browser's own bars hidden, so the bar sits
 *   underneath the URL bar until the user scrolls a page that does not scroll.
 *   `100dvh` fixes that one.
 * - Neither shrinks when the soft keyboard opens: the layout viewport keeps its
 *   full height (`interactive-widget=resizes-visual`, the default everywhere
 *   but the meta tag in index.html, which only Chrome honours). So the bar
 *   goes behind the keyboard — exactly when a key bar that exists to complete
 *   that keyboard needs to be visible.
 *
 * `visualViewport` reports what is left over, which is the number both cases
 * want. It returns null where the API does not exist, so callers keep their
 * `100dvh` class as the fallback rather than collapsing to nothing.
 */
export function useViewportHeight(): number | null {
  const [height, setHeight] = useState<number | null>(() => window.visualViewport?.height ?? null)

  useEffect(() => {
    const viewport = window.visualViewport
    if (!viewport) return

    // Pinch-zooming also shrinks the visual viewport, and resizing the page to
    // match it would fight the zoom. Only a change big enough to be browser
    // chrome or a keyboard is worth reacting to.
    const update = () => setHeight(viewport.scale > 1 ? null : viewport.height)

    update()
    viewport.addEventListener('resize', update)
    // iOS moves the visual viewport rather than resizing the layout one, so a
    // scroll can change what is on screen without a resize firing.
    viewport.addEventListener('scroll', update)
    return () => {
      viewport.removeEventListener('resize', update)
      viewport.removeEventListener('scroll', update)
    }
  }, [])

  return height
}
