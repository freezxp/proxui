import { beforeEach, describe, expect, it, vi } from 'vitest'
import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'

const apiGet = vi.fn()
const apiPost = vi.fn()
const apiDel = vi.fn()
const upload = vi.fn()

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client')
  return {
    ...actual,
    api: {
      get: (...args: unknown[]) => apiGet(...args),
      post: (...args: unknown[]) => apiPost(...args),
      del: (...args: unknown[]) => apiDel(...args),
      put: vi.fn(),
    },
    requestBlob: vi.fn().mockResolvedValue(new Blob(['x'])),
    uploadFile: (...args: unknown[]) => upload(...args),
  }
})

const { FileBrowser } = await import('./FileBrowser')

const LISTING = {
  path: '/root',
  parent: '/',
  data: [
    {
      name: 'logs',
      path: '/root/logs',
      size: 0,
      mode: 'drwxr-xr-x',
      mode_bits: 0o755,
      is_dir: true,
      is_link: false,
      mod_time: '2026-08-16T09:00:00Z',
    },
    {
      name: 'notes.txt',
      path: '/root/notes.txt',
      size: 2048,
      mode: '-rw-r--r--',
      mode_bits: 0o644,
      is_dir: false,
      is_link: false,
      mod_time: '2026-08-16T09:30:00Z',
    },
  ],
}

function renderPanel() {
  return render(
    <FileBrowser sessionId="sess-1" home="/root" onClose={() => {}} onNotice={() => {}} />,
  )
}

describe('FileBrowser', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    apiGet.mockResolvedValue(LISTING)
  })

  it('opens in the home directory the guest reported', async () => {
    renderPanel()
    await waitFor(() =>
      expect(apiGet).toHaveBeenCalledWith('/ssh-sessions/sess-1/files?path=%2Froot'),
    )
    expect(await screen.findByText(/notes\.txt/)).toBeTruthy()
    // Sizes are human-readable; 2048 bytes is not "2048". The units are the
    // shared formatter's, so a size here reads the same as one anywhere else
    // in the portal.
    expect(screen.getByText('2.0 KiB')).toBeTruthy()
  })

  it('navigates into a directory', async () => {
    const user = userEvent.setup()
    renderPanel()
    await screen.findByText(/logs/)

    apiGet.mockResolvedValue({ path: '/root/logs', parent: '/root', data: [] })
    await user.click(screen.getByRole('button', { name: /logs/ }))

    await waitFor(() =>
      expect(apiGet).toHaveBeenLastCalledWith('/ssh-sessions/sess-1/files?path=%2Froot%2Flogs'),
    )
    expect(await screen.findByText('This folder is empty.')).toBeTruthy()
  })

  it('uploads into the directory being looked at', async () => {
    const user = userEvent.setup()
    upload.mockResolvedValue({ path: '/root/deploy.sh', bytes: 12 })
    const { container } = renderPanel()
    await screen.findByText(/notes\.txt/)

    const input = container.querySelector('input[type="file"]') as HTMLInputElement
    await user.upload(input, new File(['#!/bin/sh'], 'deploy.sh', { type: 'text/plain' }))

    await waitFor(() => expect(upload).toHaveBeenCalled())
    const [path] = upload.mock.calls[0]
    // The directory and the name travel in the query string; the body is the
    // file itself, so nothing has to be buffered on the way through.
    expect(path).toBe('/ssh-sessions/sess-1/files/content?path=%2Froot&name=deploy.sh')
  })

  it('asks before deleting, and names what it would delete', async () => {
    const user = userEvent.setup()
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(false)
    renderPanel()
    await screen.findByText(/notes\.txt/)

    await user.click(screen.getAllByRole('button', { name: 'Delete' })[1])

    expect(confirm).toHaveBeenCalledWith(expect.stringContaining('/root/notes.txt'))
    expect(apiDel).not.toHaveBeenCalled()

    confirm.mockReturnValue(true)
    await user.click(screen.getAllByRole('button', { name: 'Delete' })[1])
    await waitFor(() =>
      expect(apiDel).toHaveBeenCalledWith('/ssh-sessions/sess-1/files?path=%2Froot%2Fnotes.txt'),
    )
    confirm.mockRestore()
  })

  it('says what went wrong rather than showing an empty folder', async () => {
    apiGet.mockRejectedValue(
      Object.assign(new Error('denied'), { detail: 'permission denied', name: 'ApiError' }),
    )
    renderPanel()
    await screen.findByText('The directory could not be read.')
  })
})
