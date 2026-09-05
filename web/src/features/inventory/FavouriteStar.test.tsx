import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { FavouriteStar } from './FavouriteStar'

const put = vi.fn()
const del = vi.fn()
vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: { put: (...args: unknown[]) => put(...args), del: (...args: unknown[]) => del(...args) },
  }
})

function renderStar(isFavourite: boolean) {
  const client = new QueryClient({ defaultOptions: { mutations: { retry: false } } })
  render(
    <QueryClientProvider client={client}>
      <FavouriteStar vmID="vm1" isFavourite={isFavourite} />
    </QueryClientProvider>,
  )
}

describe('FavouriteStar', () => {
  beforeEach(() => {
    put.mockReset().mockResolvedValue(undefined)
    del.mockReset().mockResolvedValue(undefined)
  })

  it('stars a VM that is not yet a favourite', async () => {
    const user = userEvent.setup()
    renderStar(false)

    await user.click(screen.getByRole('button', { name: 'Add to favourites' }))
    expect(put).toHaveBeenCalledWith('/vms/vm1/favourite')
    expect(del).not.toHaveBeenCalled()
  })

  it('unstars one that is', async () => {
    const user = userEvent.setup()
    renderStar(true)

    await user.click(screen.getByRole('button', { name: 'Remove from favourites' }))
    expect(del).toHaveBeenCalledWith('/vms/vm1/favourite')
    expect(put).not.toHaveBeenCalled()
  })

  // A star that took a round trip to light up would feel broken; one that lit
  // up and did not save would be worse.
  it('goes back to where it was when the request fails', async () => {
    const user = userEvent.setup()
    put.mockRejectedValue(new Error('nope'))
    renderStar(false)

    const button = screen.getByRole('button', { name: 'Add to favourites' })
    expect(button.getAttribute('aria-pressed')).toBe('false')

    await user.click(button)
    await waitFor(() =>
      expect(
        screen.getByRole('button', { name: 'Add to favourites' }).getAttribute('aria-pressed'),
      ).toBe('false'),
    )
  })
})
