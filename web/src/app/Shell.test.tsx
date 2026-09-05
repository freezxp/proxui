import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { Shell } from './Shell'
import type { CurrentUser, Role } from '@/api/types'

const admin: CurrentUser = {
  id: 'u1',
  username: 'jsmith',
  email: 'jsmith@example.test',
  display_name: 'Jane Smith',
  role: 'admin',
  totp_enabled: false,
  must_change_password: false,
}

let currentUser: CurrentUser = admin

vi.mock('@/features/auth/useAuth', () => ({
  useAuth: () => ({ user: currentUser, logout: vi.fn() }),
}))

vi.mock('@/lib/branding', () => ({
  useBranding: () => ({ 'branding.portal_name': 'Test portal', 'branding.logo': '' }),
}))

function renderShell(role: Role = 'admin') {
  currentUser = { ...admin, role }
  return render(
    <MemoryRouter>
      <Shell />
    </MemoryRouter>,
  )
}

describe('Shell navigation', () => {
  beforeEach(() => {
    window.localStorage.clear()
    currentUser = admin
  })

  it('starts expanded, with every destination named', () => {
    renderShell()

    expect(screen.getByRole('button', { name: 'Collapse menu' })).toBeDefined()
    for (const label of ['Dashboard', 'Inventory', 'Hosts', 'Users & groups', 'Settings']) {
      expect(screen.getByRole('link', { name: label })).toBeDefined()
    }
  })

  it('collapses, and the links keep their names for a screen reader', async () => {
    const person = userEvent.setup()
    renderShell()

    await person.click(screen.getByRole('button', { name: 'Collapse menu' }))

    // The point of the test: hiding the label visually must not leave a row of
    // unnamed icons, which is what an icon-only nav usually degrades into.
    expect(screen.getByRole('link', { name: 'Dashboard' })).toBeDefined()
    expect(screen.getByRole('link', { name: 'Users & groups' })).toBeDefined()
    expect(screen.getByRole('button', { name: 'Expand menu', expanded: false })).toBeDefined()
  })

  it('names the target in a tooltip once the label is hidden', async () => {
    const person = userEvent.setup()
    renderShell()
    await person.click(screen.getByRole('button', { name: 'Collapse menu' }))

    expect(screen.getByRole('link', { name: 'Inventory' }).getAttribute('title')).toBe('Inventory')
  })

  it('remembers the choice across a reload', async () => {
    const person = userEvent.setup()
    const { unmount } = renderShell()

    await person.click(screen.getByRole('button', { name: 'Collapse menu' }))
    unmount()
    renderShell()

    expect(screen.getByRole('button', { name: 'Expand menu' })).toBeDefined()
  })

  it('survives storage it is not allowed to write', async () => {
    const person = userEvent.setup()
    // Private-browsing modes throw here rather than no-op, and taking the shell
    // down over a remembered pane width would be a poor trade. Only this one
    // key is made to fail: everything else on the page uses storage too, and a
    // blanket failure would test something other than what is claimed.
    const real = Storage.prototype.setItem
    const setItem = vi.spyOn(Storage.prototype, 'setItem').mockImplementation(function (
      this: Storage,
      key: string,
      value: string,
    ) {
      if (key === 'proxui.nav.collapsed') throw new Error('denied')
      real.call(this, key, value)
    })

    try {
      renderShell()
      await person.click(screen.getByRole('button', { name: 'Collapse menu' }))
      expect(screen.getByRole('button', { name: 'Expand menu' })).toBeDefined()
    } finally {
      setItem.mockRestore()
    }
  })

  // The list had grown to eleven and nothing but position said which items
  // were related, so it is grouped now — and a group somebody folds shut stays
  // shut on their next visit.
  it('folds a section away and remembers it', async () => {
    const person = userEvent.setup()
    const { unmount } = renderShell()

    expect(screen.getByRole('link', { name: 'Storage' })).toBeDefined()
    await person.click(screen.getByRole('button', { name: 'Infrastructure', expanded: true }))
    expect(screen.queryByRole('link', { name: 'Storage' })).toBeNull()

    unmount()
    renderShell()
    expect(screen.queryByRole('link', { name: 'Storage' })).toBeNull()
    // The Dashboard sits above every heading, so nothing can fold it away.
    expect(screen.getByRole('link', { name: 'Dashboard' })).toBeDefined()
  })

  // Folding hides links behind a heading you can click again. The icon rail has
  // no headings, so obeying the fold there would hide links with no way back.
  it('keeps every link reachable in the icon rail, folded or not', async () => {
    const person = userEvent.setup()
    renderShell()

    await person.click(screen.getByRole('button', { name: 'Infrastructure' }))
    await person.click(screen.getByRole('button', { name: 'Collapse menu' }))

    expect(screen.getByRole('link', { name: 'Storage' })).toBeDefined()
  })

  // On a phone the sidebar is a drawer, and it has to get out of the way once
  // it has been used — otherwise it covers the page the tap just loaded.
  it('opens the menu on a phone and closes it on navigation', async () => {
    const person = userEvent.setup()
    renderShell()

    await person.click(screen.getByRole('button', { name: 'Open menu' }))
    // Both the drawer and the desktop sidebar are in the document; only the
    // viewport decides which is seen.
    expect(screen.getAllByRole('link', { name: 'Inventory' })).toHaveLength(2)

    await person.click(screen.getAllByRole('link', { name: 'Inventory' })[0])
    expect(screen.getAllByRole('link', { name: 'Inventory' })).toHaveLength(1)
  })

  it('shows an operator only what an operator may reach', () => {
    renderShell('operator')

    expect(screen.getByRole('link', { name: 'Inventory' })).toBeDefined()
    expect(screen.queryByRole('link', { name: 'Users & groups' })).toBeNull()
    expect(screen.queryByRole('link', { name: 'Hosts' })).toBeNull()
  })
})
