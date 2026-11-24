import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { renderWithAgent } from '../test/harness'
import { Settings } from './main/Settings'
import { CacheLine } from './popover/Popover'

const GB = 1024 ** 3

describe('Cache space', () => {
  it('shows the cache in three plain parts and frees what the drive has', async () => {
    renderWithAgent(<Settings />, 'empty')
    expect(await screen.findByText(/Waiting to upload/)).toBeTruthy()
    expect(screen.getByText(/Just uploaded, clearing soon/)).toBeTruthy()
    expect(screen.getByText(/Recently opened/)).toBeTruthy()
    expect(screen.getByText(/Files live on the Olsyn drive\. This PC keeps only what you opened recently and what is still uploading\./)).toBeTruthy()
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Free up space' }))
    })
    await waitFor(() => expect(screen.getByRole('status').textContent).toMatch(/^Cleared 3\.40 GB\. Those files are still on the drive\.$/))
  })

  it('adds one quiet popover line only above 1 GiB', () => {
    const cache = { used_bytes: 0.5 * GB, cap_bytes: 10 * GB, unsent_bytes: 0, uploaded_bytes: 0, opened_bytes: 0.5 * GB }
    const { rerender } = render(<CacheLine cache={cache} onOpen={() => undefined} />)
    expect(screen.queryByRole('button')).toBeNull()
    rerender(<CacheLine cache={{ ...cache, used_bytes: 3.1 * GB, unsent_bytes: 1.2 * GB }} onOpen={() => undefined} />)
    expect(screen.getByRole('button').textContent).toBe('3.10 GB kept on this PC: 1.20 GB still uploading.')
    rerender(<CacheLine cache={{ ...cache, used_bytes: 3.1 * GB, uploaded_bytes: 2 * GB }} onOpen={() => undefined} />)
    expect(screen.getByRole('button').textContent).toBe('3.10 GB kept on this PC: uploaded files clear soon.')
    rerender(<CacheLine cache={undefined} onOpen={() => undefined} />)
    expect(screen.queryByRole('button')).toBeNull()
  })
})
