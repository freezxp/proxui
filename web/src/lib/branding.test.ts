import { describe, expect, it } from 'vitest'
import { resolvePortalName } from './branding'

describe('resolvePortalName', () => {
  it('prefers a name someone chose', () => {
    expect(resolvePortalName('exstudios.vm', 'proxui.internal')).toBe('exstudios.vm')
  })

  it('falls back to the address the browser used', () => {
    expect(resolvePortalName('', 'vm.example.com')).toBe('vm.example.com')
  })

  it('treats whitespace as unset rather than as a name', () => {
    expect(resolvePortalName('   ', 'vm.example.com')).toBe('vm.example.com')
  })

  it('uses an IP when that is how the portal was reached', () => {
    // Literal on purpose: an operator who typed an address should see it back,
    // not a guess at what they meant.
    expect(resolvePortalName('', '192.168.100.23')).toBe('192.168.100.23')
  })

  it('has something to show even with no hostname at all', () => {
    expect(resolvePortalName('', '')).toBe('ProxUI')
  })
})
