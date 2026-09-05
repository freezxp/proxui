import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { FolderSidebar, type FolderSelection } from './FolderSidebar'

const get = vi.fn()
const post = vi.fn()
const patch = vi.fn()
const del = vi.fn()
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: {
      get: (...a: unknown[]) => get(...a),
      post: (...a: unknown[]) => post(...a),
      patch: (...a: unknown[]) => patch(...a),
      del: (...a: unknown[]) => del(...a),
    },
  }
})

function renderSidebar(selection: FolderSelection = { kind: 'all' }) {
  const onSelect = vi.fn()
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  })
  render(
    <QueryClientProvider client={client}>
      <FolderSidebar selection={selection} onSelect={onSelect} totalVMs={35} />
    </QueryClientProvider>,
  )
  return { onSelect }
}

describe('FolderSidebar', () => {
  beforeEach(() => {
    get.mockReset().mockResolvedValue({
      data: [{ id: 'f1', name: 'Production', position: 0, vm_count: 12, created_at: '' }],
    })
    post.mockReset().mockResolvedValue({ id: 'f2', name: 'Staging' })
    patch.mockReset().mockResolvedValue(undefined)
    del.mockReset().mockResolvedValue(undefined)
  })

  it('reports which node was chosen', async () => {
    const user = userEvent.setup()
    const { onSelect } = renderSidebar()

    await user.click(screen.getByRole('button', { name: /All VMs/ }))
    expect(onSelect).toHaveBeenCalledWith({ kind: 'all' })

    await user.click(screen.getByRole('button', { name: /Favourites/ }))
    expect(onSelect).toHaveBeenCalledWith({ kind: 'favourites' })

    await user.click(screen.getByRole('button', { name: /Unfiled/ }))
    expect(onSelect).toHaveBeenCalledWith({ kind: 'unfiled' })

    // Anchored: "Rename Production" and "Delete Production" also contain the
    // folder's name, and only the node itself starts with it.
    await waitFor(() => screen.getByRole('button', { name: /^Production/ }))
    await user.click(screen.getByRole('button', { name: /^Production/ }))
    expect(onSelect).toHaveBeenCalledWith({ kind: 'folder', id: 'f1' })
  })

  it('shows how many VMs a folder holds', async () => {
    renderSidebar()
    await waitFor(() => expect(screen.getByText('(12)')).toBeDefined())
  })

  // "Delete folder" reads like it takes the machines with it. It does not, and
  // the confirmation has to say so before anybody finds out the hard way.
  it('asks before deleting and says the VMs stay', async () => {
    const user = userEvent.setup()
    renderSidebar()
    await waitFor(() => screen.getByRole('button', { name: 'Delete Production' }))

    await user.click(screen.getByRole('button', { name: 'Delete Production' }))
    expect(del).not.toHaveBeenCalled()
    expect(screen.getByText(/VMs stay/)).toBeDefined()

    await user.click(screen.getByRole('button', { name: 'No' }))
    expect(del).not.toHaveBeenCalled()

    await user.click(screen.getByRole('button', { name: 'Delete Production' }))
    await user.click(screen.getByRole('button', { name: 'Yes' }))
    expect(del).toHaveBeenCalledWith('/folders/f1')
  })

  it('renames on Enter and refuses an empty name', async () => {
    const user = userEvent.setup()
    renderSidebar()
    await waitFor(() => screen.getByRole('button', { name: 'Rename Production' }))

    await user.click(screen.getByRole('button', { name: 'Rename Production' }))
    const field = screen.getByDisplayValue('Production')

    await user.clear(field)
    await user.keyboard('{Enter}')
    expect(patch).not.toHaveBeenCalled()

    await user.type(field, 'Prod')
    await user.keyboard('{Enter}')
    expect(patch).toHaveBeenCalledWith('/folders/f1', { name: 'Prod', position: 0 })
  })

  it('opens a folder it has just created', async () => {
    const user = userEvent.setup()
    const { onSelect } = renderSidebar()

    await user.click(screen.getByRole('button', { name: '+ New folder' }))
    await user.type(screen.getByPlaceholderText('Folder name'), 'Staging')
    await user.keyboard('{Enter}')

    await waitFor(() => expect(post).toHaveBeenCalledWith('/folders', { name: 'Staging' }))
    await waitFor(() => expect(onSelect).toHaveBeenCalledWith({ kind: 'folder', id: 'f2' }))
  })
})
