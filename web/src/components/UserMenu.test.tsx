import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { UserMenu } from './UserMenu'
import type { CurrentUser } from '@/api/types'

const user: CurrentUser = {
  id: 'u1',
  username: 'jsmith',
  email: 'jsmith@example.test',
  display_name: 'Jane Smith',
  role: 'operator',
  totp_enabled: false,
  must_change_password: false,
}

function renderMenu(overrides: Partial<Parameters<typeof UserMenu>[0]> = {}) {
  const onChangePassword = vi.fn()
  const onTwoFactor = vi.fn()
  const onSignOut = vi.fn()
  render(
    <UserMenu
      user={user}
      onChangePassword={onChangePassword}
      onTwoFactor={onTwoFactor}
      onSignOut={onSignOut}
      {...overrides}
    />,
  )
  return { onChangePassword, onTwoFactor, onSignOut }
}

describe('UserMenu', () => {
  it('keeps its contents closed until asked', () => {
    renderMenu()
    expect(screen.getByRole('button', { expanded: false })).toBeDefined()
    expect(screen.queryByRole('menu')).toBeNull()
  })

  it('opens, runs an action, and closes again', async () => {
    const person = userEvent.setup()
    const { onChangePassword } = renderMenu()

    await person.click(screen.getByRole('button', { expanded: false }))
    expect(screen.getByRole('menu')).toBeDefined()

    await person.click(screen.getByRole('menuitem', { name: 'Change password' }))
    expect(onChangePassword).toHaveBeenCalledOnce()
    // A menu that stays open over the dialog it just opened is a menu in the way.
    expect(screen.queryByRole('menu')).toBeNull()
  })

  it('closes on Escape and returns focus to the trigger', async () => {
    const person = userEvent.setup()
    renderMenu()

    const trigger = screen.getByRole('button', { expanded: false })
    await person.click(trigger)
    await person.keyboard('{Escape}')

    expect(screen.queryByRole('menu')).toBeNull()
    // Leaving focus at the top of the document would strand a keyboard user.
    expect(document.activeElement).toBe(trigger)
  })

  it('closes when the pointer goes elsewhere', async () => {
    const person = userEvent.setup()
    renderMenu()

    await person.click(screen.getByRole('button', { expanded: false }))
    await person.click(document.body)
    expect(screen.queryByRole('menu')).toBeNull()
  })

  it('shows who is signed in and their role', async () => {
    const person = userEvent.setup()
    renderMenu()

    await person.click(screen.getByRole('button', { expanded: false }))
    expect(screen.getByText('jsmith@example.test')).toBeDefined()
    expect(screen.getByText('Operator')).toBeDefined()
  })

  it('marks the active theme so the choice is visible', async () => {
    const person = userEvent.setup()
    renderMenu()

    await person.click(screen.getByRole('button', { expanded: false }))
    await person.click(screen.getByRole('button', { name: 'Dark' }))

    expect(screen.getByRole('button', { name: 'Dark', pressed: true })).toBeDefined()
    expect(document.documentElement.classList.contains('dark')).toBe(true)
  })

  it('signs out from the menu', async () => {
    const person = userEvent.setup()
    const { onSignOut } = renderMenu()

    await person.click(screen.getByRole('button', { expanded: false }))
    await person.click(screen.getByRole('menuitem', { name: 'Sign out' }))
    expect(onSignOut).toHaveBeenCalledOnce()
  })
})
