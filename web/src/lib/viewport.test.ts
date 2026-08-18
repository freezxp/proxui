import { afterEach, describe, expect, it } from 'vitest'
import { act, renderHook } from '@testing-library/react'
import { useViewportHeight } from './viewport'

/**
 * The reason this hook exists is a bar that was below the fold on a phone, so
 * what matters is that it reports the shrunken height and that it stops
 * reporting when it would be wrong to resize the page.
 */

type Listener = () => void

function fakeViewport(height: number, scale = 1) {
  const listeners: Record<string, Listener[]> = {}
  const viewport = {
    height,
    scale,
    addEventListener: (type: string, fn: Listener) => {
      ;(listeners[type] ??= []).push(fn)
    },
    removeEventListener: (type: string, fn: Listener) => {
      listeners[type] = (listeners[type] ?? []).filter((l) => l !== fn)
    },
    emit(type: string) {
      for (const fn of listeners[type] ?? []) fn()
    },
    listenerCount: () => Object.values(listeners).flat().length,
  }
  Object.defineProperty(window, 'visualViewport', { value: viewport, configurable: true })
  return viewport
}

afterEach(() => {
  Object.defineProperty(window, 'visualViewport', { value: undefined, configurable: true })
})

describe('useViewportHeight', () => {
  it('reports nothing where the browser has no visual viewport', () => {
    const { result } = renderHook(() => useViewportHeight())
    expect(result.current).toBeNull()
  })

  it('follows the visible height when the soft keyboard opens and closes', () => {
    const viewport = fakeViewport(800)
    const { result } = renderHook(() => useViewportHeight())
    expect(result.current).toBe(800)

    act(() => {
      viewport.height = 420
      viewport.emit('resize')
    })
    expect(result.current).toBe(420)

    act(() => {
      viewport.height = 800
      viewport.emit('scroll')
    })
    expect(result.current).toBe(800)
  })

  it('stands aside while the page is pinch-zoomed', () => {
    const viewport = fakeViewport(800)
    const { result } = renderHook(() => useViewportHeight())

    act(() => {
      viewport.scale = 2
      viewport.height = 300
      viewport.emit('resize')
    })
    expect(result.current).toBeNull()
  })

  it('unsubscribes on unmount', () => {
    const viewport = fakeViewport(800)
    const { unmount } = renderHook(() => useViewportHeight())
    expect(viewport.listenerCount()).toBeGreaterThan(0)
    unmount()
    expect(viewport.listenerCount()).toBe(0)
  })
})
