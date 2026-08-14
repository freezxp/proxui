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
