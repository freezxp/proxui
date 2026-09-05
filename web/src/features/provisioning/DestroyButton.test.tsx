import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { MemoryRouter } from 'react-router-dom'
import { DestroyButton } from './DestroyButton'

const del = vi.fn()
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return { ...actual, api: { del: (...args: unknown[]) => del(...args) } }
})

function renderButton(state = 'stopped') {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } })
  render(
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <DestroyButton vmID="vm1" name="web-01" state={state} />
      </QueryClientProvider>
    </MemoryRouter>,
  )
}

describe('DestroyButton', () => {
  beforeEach(() => {
    del.mockReset().mockResolvedValue({ request_id: 'req1', state: 'pending' })
  })

  // The server checks this too, and that is where the control really lives.
  // What the dialog owes the operator is not being able to click through it by
  // accident.
  it('stays disabled until the name is typed exactly', async () => {
    const user = userEvent.setup()
    renderButton()
    await user.click(screen.getByRole('button', { name: 'Destroy' }))

    const confirm = screen.getByRole('button', { name: 'Destroy permanently' })
    expect(confirm.hasAttribute('disabled')).toBe(true)

    const field = screen.getByRole('textbox')
    await user.type(field, 'web-0')
    expect(confirm.hasAttribute('disabled')).toBe(true)

    await user.type(field, '1')
    expect(confirm.hasAttribute('disabled')).toBe(false)

    await user.click(confirm)
    expect(del).toHaveBeenCalledWith('/vms/vm1', { confirm_name: 'web-01' })
  })

  it('does not accept a different guest’s name', async () => {
    const user = userEvent.setup()
    renderButton()
    await user.click(screen.getByRole('button', { name: 'Destroy' }))
    await user.type(screen.getByRole('textbox'), 'web-02')

    expect(
      screen.getByRole('button', { name: 'Destroy permanently' }).hasAttribute('disabled'),
    ).toBe(true)
    expect(del).not.toHaveBeenCalled()
  })

  // The platform refuses to remove a running guest; saying so up front beats
  // letting someone type a name and then reading a rejection.
  it('warns when the guest is still running', async () => {
    const user = userEvent.setup()
    renderButton('running')
    await user.click(screen.getByRole('button', { name: 'Destroy' }))

    expect(screen.getByText(/refuse to destroy it until it is stopped/)).toBeDefined()
  })
})
