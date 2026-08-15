import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { PowerControls } from './PowerControls'
import { ApiError } from '@/api/client'
import type { VMState } from '@/api/types'

const post = vi.fn()
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return { ...actual, api: { post: (...args: unknown[]) => post(...args) } }
})

function renderControls(state: VMState) {
  const onRequested = vi.fn()
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } })
  render(
    <QueryClientProvider client={client}>
      <PowerControls vmId="vm1" state={state} onRequested={onRequested} />
    </QueryClientProvider>,
  )
  return { onRequested }
}

describe('PowerControls', () => {
  beforeEach(() => {
    post.mockReset().mockResolvedValue({ task_id: 'UPID:...', status: 'accepted' })
  })

  it('offers only what makes sense for the state', () => {
    renderControls('stopped')
    expect(screen.getByRole('button', { name: 'Start' })).toBeDefined()
    expect(screen.queryByRole('button', { name: 'Force stop' })).toBeNull()
  })

  it('offers the graceful pair and the forceful one when running', () => {
    renderControls('running')
    for (const name of ['Shut down', 'Reboot', 'Force stop']) {
      expect(screen.getByRole('button', { name })).toBeDefined()
    }
    expect(screen.queryByRole('button', { name: 'Start' })).toBeNull()
  })

  // Starting a machine cannot lose anything, so making it a two-step is
  // friction with no safety bought.
  it('starts on the first click', async () => {
    const person = userEvent.setup()
    const { onRequested } = renderControls('stopped')

    await person.click(screen.getByRole('button', { name: 'Start' }))

    expect(post).toHaveBeenCalledWith('/vms/vm1/power', { action: 'start' })
    await vi.waitFor(() => expect(onRequested).toHaveBeenCalledWith('start'))
  })

  // The whole point of the confirm step: everything that interrupts service
  // must survive a misclick.
  it.each([
    ['Shut down', 'shutdown'],
    ['Reboot', 'reboot'],
    ['Force stop', 'stop'],
  ])('asks before it %ss', async (label, action) => {
    const person = userEvent.setup()
    renderControls('running')

    await person.click(screen.getByRole('button', { name: label }))
    expect(post).not.toHaveBeenCalled()

    await person.click(within(screen.getByRole('dialog')).getByRole('button', { name: label }))
    expect(post).toHaveBeenCalledWith('/vms/vm1/power', { action })
  })

  it('sends nothing when the confirmation is dismissed', async () => {
    const person = userEvent.setup()
    const { onRequested } = renderControls('running')

    await person.click(screen.getByRole('button', { name: 'Force stop' }))
    await person.click(screen.getByRole('button', { name: 'Cancel' }))

    expect(post).not.toHaveBeenCalled()
    expect(onRequested).not.toHaveBeenCalled()
    expect(screen.queryByRole('dialog')).toBeNull()
  })

  // A platform token without VM.PowerMgmt is fixed on the cluster, not in the
  // portal, so the message has to say which of the two is wrong.
  it('names a platform refusal rather than reporting a generic failure', async () => {
    const person = userEvent.setup()
    post.mockRejectedValue(
      new ApiError(502, 'platform.permission_denied', 'The platform credential is not allowed.'),
    )
    const { onRequested } = renderControls('stopped')

    await person.click(screen.getByRole('button', { name: 'Start' }))

    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'The platform credential is not allowed to perform power actions.',
    )
    expect(onRequested).not.toHaveBeenCalled()
  })
})
