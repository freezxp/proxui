import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { PublishDialog } from './PublishDialog'
import { ApiError } from '@/api/client'

const get = vi.fn()
const post = vi.fn()
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: {
      get: (...args: unknown[]) => get(...args),
      post: (...args: unknown[]) => post(...args),
    },
  }
})

const inventory = {
  data: [
    { id: 'vm-1', name: 'beszel', state: 'running', ip_addresses: ['10.0.29.177'] },
    { id: 'vm-2', name: 'zitadel', state: 'running', ip_addresses: ['192.168.100.20', '10.1.1.1'] },
    // Without an address there is nothing to point a rule at, so this one must
    // not be offered — and the reason has to be visible.
    { id: 'vm-3', name: 'no-agent', state: 'running', ip_addresses: [] },
  ],
}

function renderDialog() {
  const onPublished = vi.fn()
  const onClose = vi.fn()
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <PublishDialog providerID="p1" version={34} onClose={onClose} onPublished={onPublished} />
    </QueryClientProvider>,
  )
  return { onPublished, onClose }
}

describe('PublishDialog', () => {
  beforeEach(() => {
    get.mockReset().mockResolvedValue(inventory)
    post.mockReset().mockResolvedValue({ id: 'app-1', hostname: 'app.example.com' })
  })

  it('offers VM picking by default, since that is what the dashboard cannot do', async () => {
    renderDialog()
    expect(await screen.findByLabelText('Virtual machine')).toBeDefined()
    expect(
      screen.getByRole('button', { name: 'A VM in the inventory', pressed: true }),
    ).toBeDefined()
  })

  it('lists only VMs the portal has an address for, and says why', async () => {
    renderDialog()
    const picker = (await screen.findByLabelText('Virtual machine')) as HTMLSelectElement

    const names = Array.from(picker.options).map((o) => o.textContent)
    expect(names.some((n) => n?.includes('beszel'))).toBe(true)
    expect(names.some((n) => n?.includes('no-agent'))).toBe(false)

    // An empty-looking list with no explanation is the failure to avoid.
    expect(await screen.findByText(/not listed because the portal has no address/)).toBeDefined()
  })

  // PUB-43. The most consequential thing this panel does, so it cannot be a
  // default that someone clicks past.
  it('will not publish until the exposure is acknowledged', async () => {
    const person = userEvent.setup()
    renderDialog()

    await person.selectOptions(await screen.findByLabelText('Virtual machine'), 'vm-1')
    await person.type(screen.getByLabelText('Port'), '3000')
    await person.type(screen.getByLabelText('Public hostname'), 'app.example.com')

    expect(screen.getByRole('button', { name: 'Publish' })).toHaveProperty('disabled', true)

    await person.click(screen.getByRole('checkbox'))
    expect(screen.getByRole('button', { name: 'Publish' })).toHaveProperty('disabled', false)
  })

  it('sends the chosen VM, its address and the version it read', async () => {
    const person = userEvent.setup()
    const { onPublished } = renderDialog()

    await person.selectOptions(await screen.findByLabelText('Virtual machine'), 'vm-1')
    await person.type(screen.getByLabelText('Port'), '3000')
    await person.type(screen.getByLabelText('Public hostname'), 'app.example.com')
    await person.click(screen.getByRole('checkbox'))
    await person.click(screen.getByRole('button', { name: 'Publish' }))

    await waitFor(() => expect(post).toHaveBeenCalled())
    const [path, body] = post.mock.calls[0]
    expect(path).toBe('/edge-providers/p1/apps')
    expect(body).toMatchObject({
      hostname: 'app.example.com',
      address: '10.0.29.177',
      port: 3000,
      vm_id: 'vm-1',
      acknowledge_exposure: true,
      // Carried so a concurrent change is refused rather than clobbered.
      read_version: 34,
    })
    await waitFor(() => expect(onPublished).toHaveBeenCalled())
  })

  it('falls back to a free-text address for anything outside the inventory', async () => {
    const person = userEvent.setup()
    renderDialog()

    await person.click(screen.getByRole('button', { name: 'An address' }))
    await person.type(screen.getByLabelText('Address'), '10.0.13.9')
    await person.type(screen.getByLabelText('Port'), '8080')
    await person.type(screen.getByLabelText('Public hostname'), 'db.example.com')
    await person.click(screen.getByRole('checkbox'))
    await person.click(screen.getByRole('button', { name: 'Publish' }))

    await waitFor(() => expect(post).toHaveBeenCalled())
    expect(post.mock.calls[0][1]).toMatchObject({ address: '10.0.13.9', vm_id: '' })
  })

  // A stale read means somebody else changed the table while this was open.
  // Retrying the same thing would clobber them, so the message says to reload.
  it('explains a stale read rather than inviting a retry', async () => {
    const person = userEvent.setup()
    post.mockRejectedValue(new ApiError(409, 'publish.stale', 'it was version 33 and is now 34'))
    renderDialog()

    await person.click(screen.getByRole('button', { name: 'An address' }))
    await person.type(screen.getByLabelText('Address'), '10.0.13.9')
    await person.type(screen.getByLabelText('Port'), '8080')
    await person.type(screen.getByLabelText('Public hostname'), 'db.example.com')
    await person.click(screen.getByRole('checkbox'))
    await person.click(screen.getByRole('button', { name: 'Publish' }))

    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'The routing table changed while this dialog was open. Close it, reload, and try again.',
    )
  })

  // A missing write scope is fixed on Cloudflare, not in the portal, so the
  // server's wording is passed through rather than replaced.
  it('passes through the reason a write was refused', async () => {
    const person = userEvent.setup()
    post.mockRejectedValue(
      new ApiError(
        502,
        'publish.write_not_permitted',
        'The API token appears to lack write permission.',
      ),
    )
    renderDialog()

    await person.click(screen.getByRole('button', { name: 'An address' }))
    await person.type(screen.getByLabelText('Address'), '10.0.13.9')
    await person.type(screen.getByLabelText('Port'), '8080')
    await person.type(screen.getByLabelText('Public hostname'), 'db.example.com')
    await person.click(screen.getByRole('checkbox'))
    await person.click(screen.getByRole('button', { name: 'Publish' }))

    expect(await screen.findByRole('alert')).toHaveProperty(
      'textContent',
      'The API token appears to lack write permission.',
    )
  })

  it('shows what will be written before it is written', async () => {
    const person = userEvent.setup()
    renderDialog()

    await person.selectOptions(await screen.findByLabelText('Virtual machine'), 'vm-2')
    await person.type(screen.getByLabelText('Port'), '8080')
    await person.type(screen.getByLabelText('Public hostname'), 'id.example.com')

    // The first address is the one used, and saying so beats finding out later.
    expect(screen.getByText(/id\.example\.com → http:\/\/192\.168\.100\.20:8080/)).toBeDefined()
    expect(screen.getByText(/several addresses/)).toBeDefined()
  })
})
