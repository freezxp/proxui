/** jsdom implements no media queries, and the theme code asks the operating
 *  system what it prefers. A stub that reports "light" keeps the tests
 *  deterministic; real browsers all provide the real thing. */
if (!window.matchMedia) {
  window.matchMedia = ((query: string) => ({
    matches: false,
    media: query,
    onchange: null,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
    dispatchEvent: () => false,
  })) as unknown as typeof window.matchMedia
}

/** uPlot measures its container with a ResizeObserver, which jsdom does not
 *  implement. A no-op stub lets chart components mount in a test; the canvas
 *  they draw is not asserted on (jsdom renders none), only the wiring around
 *  it. */
if (!globalThis.ResizeObserver) {
  globalThis.ResizeObserver = class {
    observe() {}
    unobserve() {}
    disconnect() {}
  } as unknown as typeof ResizeObserver
}
