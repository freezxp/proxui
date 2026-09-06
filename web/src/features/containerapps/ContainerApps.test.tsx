import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { ContainerAppsPage } from './ContainerAppsPage'

const get = vi.fn()
const post = vi.fn()
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: { get: (...a: unknown[]) => get(...a), post: (...a: unknown[]) => post(...a) },
  }
})

const upstream = {
  scripts_repo: 'community-scripts/ProxmoxVE',
  scripts_ref: '08fdd8875172abcd3c167f13a00bdb65fcb0e61e',
  engine_repo: 'community-scripts/core',
  engine_ref: 'b7ddecf9f0ddc88781657aff407b78867472ebd5',
}

const catalogue = {
  data: [
    { id: 'adguard', name: 'Adguard', tags: ['adblock'], cores: 1, memory_mb: 512, disk_gb: 2 },
    { id: 'jellyfin', name: 'Jellyfin', tags: ['media'], cores: 2, memory_mb: 2048, disk_gb: 8 },
  ],
  tags: ['adblock', 'media'],
  upstream,
}

function renderPage() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <ContainerAppsPage />
    </QueryClientProvider>,
  )
}

describe('Container apps', () => {
  beforeEach(() => {
    post.mockReset().mockResolvedValue({ id: 'd1', state: 'pending' })
    get.mockReset().mockImplementation((path: string) => {
      if (path === '/container-apps') return Promise.resolve(catalogue)
      if (path === '/container-deployments') return Promise.resolve({ data: [] })
      if (path === '/platforms') return Promise.resolve({ data: [{ id: 'p1', name: 'pve-home' }] })
      if (path.endsWith('/readiness')) return Promise.resolve({ nodes: [{ node: 'cx1' }] })
      return Promise.resolve({ data: [] })
    })
  })

  it('lists the catalogue and narrows it by search', async () => {
    const user = userEvent.setup()
    renderPage()

    await waitFor(() => screen.getByText('Adguard'))
    expect(screen.getByText('Jellyfin')).toBeTruthy()

    await user.type(screen.getByPlaceholderText(/Search 2 applications/), 'jelly')
    expect(screen.queryByText('Adguard')).toBeNull()
    expect(screen.getByText('Jellyfin')).toBeTruthy()
  })

  // The pinned commits are the whole reason this is reviewable, so the page
  // says which ones it shipped with rather than leaving it to the ADR.
  it('says which upstream commits it shipped with', async () => {
    renderPage()
    await waitFor(() => screen.getByText(/community-scripts\/ProxmoxVE@08fdd88/))
  })

  // The portal is asking to run a large third-party program as root. The honest
  // way to ask is to show what will run.
  it('shows the command before it runs, and posts an identifier', async () => {
    const user = userEvent.setup()
    renderPage()

    await waitFor(() => screen.getByText('Adguard'))
    await user.click(screen.getByText('Adguard'))

    // Pinned URLs, not a branch, and the vendored script rather than a curl.
    await waitFor(() => screen.getByText(/COMMUNITY_SCRIPTS_URL/))
    const shown = screen.getByText(/COMMUNITY_SCRIPTS_URL/).textContent ?? ''
    expect(shown).toContain(upstream.scripts_ref)
    expect(shown).toContain(upstream.engine_ref)
    expect(shown).toContain('vendored with ProxUI')
    expect(shown).not.toContain('/main/')

    await user.selectOptions(screen.getByLabelText('Platform'), 'p1')
    await waitFor(() => screen.getByRole('option', { name: 'cx1' }))
    await user.selectOptions(screen.getByLabelText('Node'), 'cx1')
    await user.click(screen.getByRole('button', { name: 'Deploy to cx1' }))

    await waitFor(() => expect(post).toHaveBeenCalled())
    const [path, body] = post.mock.calls[0] as [string, Record<string, unknown>]
    expect(path).toBe('/platforms/p1/container-deployments')
    // An identifier and settings. Nothing here could be a command.
    expect(body.app_id).toBe('adguard')
    expect(body.node).toBe('cx1')
    expect(body.cores).toBe(1)
  })

  // Nothing can be deployed until there is somewhere to deploy it, and the
  // button says which node it is about to act on.
  it('will not deploy without a platform and a node', async () => {
    const user = userEvent.setup()
    renderPage()

    await waitFor(() => screen.getByText('Adguard'))
    await user.click(screen.getByText('Adguard'))

    const button = () => screen.getByRole('button', { name: /^Deploy to/ })
    expect(button().hasAttribute('disabled')).toBe(true)
    expect(button().textContent).toContain('a node')

    await user.selectOptions(screen.getByLabelText('Platform'), 'p1')
    await waitFor(() => screen.getByRole('option', { name: 'cx1' }))
    await user.selectOptions(screen.getByLabelText('Node'), 'cx1')
    expect(button().hasAttribute('disabled')).toBe(false)
  })
})
