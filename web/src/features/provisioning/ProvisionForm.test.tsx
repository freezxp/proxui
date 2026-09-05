import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ProvisionForm } from './ProvisionForm'

const get = vi.fn()
const post = vi.fn()
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: { get: (...a: unknown[]) => get(...a), post: (...a: unknown[]) => post(...a) },
  }
})

/** Waits for an async query's options before choosing one. */
async function choose(
  user: ReturnType<typeof userEvent.setup>,
  label: string,
  value: string,
  option: string,
) {
  await waitFor(() => screen.getByRole('option', { name: option }))
  await user.selectOptions(screen.getByLabelText(label), value)
}

function renderForm() {
  const onStarted = vi.fn()
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <ProvisionForm onClose={vi.fn()} onStarted={onStarted} />
    </QueryClientProvider>,
  )
  return { onStarted }
}

describe('ProvisionForm', () => {
  beforeEach(() => {
    post.mockReset().mockResolvedValue({ request_id: 'req1', state: 'pending' })
    get.mockReset().mockImplementation((path: string) => {
      if (path === '/platforms') return Promise.resolve({ data: [{ id: 'p1', name: 'pve-home' }] })
      if (path === '/vm-groups')
        return Promise.resolve({ data: [{ id: 'g1', name: 'Web servers' }] })
      if (path === '/ssh-key')
        return Promise.resolve({ exists: true, public_key: 'ssh-ed25519 AAAA portal' })
      if (path.endsWith('/templates')) {
        return Promise.resolve({
          data: [
            {
              external_id: '9000',
              name: 'debian-13',
              type: 'qemu',
              node: 'pve',
              disk_bytes: 0,
              has_cloud_init: true,
            },
          ],
        })
      }
      return Promise.resolve({ data: [] })
    })
  })

  // The form is opened from the inventory now, so it cannot assume a platform:
  // a template belongs to a cluster, and everything after that depends on it.
  it('asks for a platform before offering templates', async () => {
    const user = userEvent.setup()
    renderForm()

    await waitFor(() => screen.getByText(/Pick a platform/))
    expect(get).not.toHaveBeenCalledWith(expect.stringContaining('/templates'))

    await choose(user, 'Platform', 'p1', 'pve-home')
    await waitFor(() => expect(get).toHaveBeenCalledWith('/platforms/p1/templates'))
  })

  // Filing the guest into a VM group is what makes it visible to anyone but an
  // administrator. The API has taken it since provisioning shipped and nothing
  // offered it until the form moved here.
  it('sends the VM group and the target node', async () => {
    const user = userEvent.setup()
    renderForm()

    await choose(user, 'Platform', 'p1', 'pve-home')
    await choose(user, 'Template', '9000', 'debian-13 (pve)')
    await user.type(screen.getByLabelText('Name'), 'web-02')
    await choose(user, 'VM group', 'g1', 'Web servers')

    await user.click(screen.getByRole('button', { name: 'Create' }))

    await waitFor(() => expect(post).toHaveBeenCalled())
    const [path, body] = post.mock.calls[0] as [string, Record<string, unknown>]
    expect(path).toBe('/platforms/p1/provision')
    expect(body.vm_group_id).toBe('g1')
    // The node defaults to the template's own, which is the only one a linked
    // clone can use.
    expect(body.node).toBe('pve')
    expect(body.name).toBe('web-02')
  })

  it('will not submit without a platform, a template and a name', async () => {
    const user = userEvent.setup()
    renderForm()

    const create = () => screen.getByRole('button', { name: 'Create' })
    expect(create().hasAttribute('disabled')).toBe(true)

    await choose(user, 'Platform', 'p1', 'pve-home')
    expect(create().hasAttribute('disabled')).toBe(true)

    await choose(user, 'Template', '9000', 'debian-13 (pve)')
    expect(create().hasAttribute('disabled')).toBe(true)

    await user.type(screen.getByLabelText('Name'), 'web-02')
    expect(create().hasAttribute('disabled')).toBe(false)
  })
})
