import { act, fireEvent, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { App } from '../App'
import { renderWithAgent } from '../test/harness'

describe('when security software removed or blocked Olsyn Drive', () => {
  it('says so in plain words, says the waiting files are safe, and links to what to do', async () => {
    const { shell } = renderWithAgent(<App surface="main" />, 'removed')
    await act(async () => undefined)
    expect(await screen.findByRole('heading', { name: 'Windows Security removed or blocked Olsyn Drive on this PC.' })).toBeTruthy()
    expect(screen.getByText(/Your files waiting to upload are safe/)).toBeTruthy()
    expect(screen.queryByText("Olsyn Drive isn't running.")).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'What to do' }))
    expect(shell.opened[0]).toMatch(/docs\/pre-release-windows\.md$/)
  })
})
