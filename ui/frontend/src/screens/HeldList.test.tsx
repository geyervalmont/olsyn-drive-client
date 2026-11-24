import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { useAgentView } from '../state/context'
import { renderWithAgent, testShell } from '../test/harness'
import { HeldList } from './main/HeldList'

function Live() {
  const view = useAgentView()
  return <HeldList items={view.held} />
}

describe('HeldList', () => {
  it('says why each file is held, in plain words, with what to do', async () => {
    renderWithAgent(<Live />, 'held')
    const rows = await screen.findAllByRole('listitem')
    expect(rows).toHaveLength(4)
    expect(screen.getByText('Managed folder')).toBeTruthy()
    expect(screen.getByText('No permission to add here')).toBeTruthy()
    expect(screen.getByText('Name not allowed')).toBeTruthy()
    expect(screen.getByText('Damaged on the way')).toBeTruthy()
    expect(screen.getAllByRole('button', { name: 'Re-upload' })).toHaveLength(1)
    expect(screen.getByText(/cannot store a name that ends with a dot/)).toBeTruthy()
    for (const row of rows) {
      expect(within(row).getByRole('button', { name: /Save a copy/ })).toBeTruthy()
      expect(within(row).getByRole('button', { name: 'Discard' })).toBeTruthy()
    }
    // The full name is available even when the row cuts it.
    expect(screen.getByTitle('Lobby option final.')).toBeTruthy()
  })

  it('asks before discarding, then removes the item', async () => {
    renderWithAgent(<Live />, 'held')
    const row = (await screen.findAllByRole('listitem'))[0]
    const name = within(row).getAllByText(/\./)[0].closest('[title]')!.getAttribute('title')!
    fireEvent.click(within(row).getByRole('button', { name: 'Discard' }))
    const dialog = screen.getByRole('dialog', { name: 'Discard ' + name + '?' })
    expect(within(dialog).getByText(/can't be undone/)).toBeTruthy()
    // "Keep it" closes without discarding.
    fireEvent.click(within(dialog).getByRole('button', { name: 'Keep it' }))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.getAllByRole('listitem')).toHaveLength(4)

    fireEvent.click(within(row).getByRole('button', { name: 'Discard' }))
    await act(async () => {
      fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Discard' }))
    })
    await waitFor(() => expect(screen.getAllByRole('listitem')).toHaveLength(3))
    expect(screen.queryByRole('dialog')).toBeNull()
    expect(screen.getByText('Discarded ' + name)).toBeTruthy()
  })

  it('saves a copy to a folder the person picks', async () => {
    const shell = testShell('C:\\Users\\ada\\Desktop')
    renderWithAgent(<Live />, 'held', shell)
    const row = (await screen.findAllByRole('listitem'))[0]
    await act(async () => {
      fireEvent.click(within(row).getByRole('button', { name: /Save a copy/ }))
    })
    await waitFor(() => expect(screen.getAllByRole('listitem')).toHaveLength(3))
    expect(shell.picked[0]).toMatch(/^Save a copy of /)
    expect(screen.getByText(/C:\\Users\\ada\\Desktop\\/)).toBeTruthy()
  })

  it('does nothing when the folder picker is cancelled', async () => {
    renderWithAgent(<Live />, 'held', testShell(null))
    const row = (await screen.findAllByRole('listitem'))[0]
    await act(async () => {
      fireEvent.click(within(row).getByRole('button', { name: /Save a copy/ }))
    })
    expect(screen.getAllByRole('listitem')).toHaveLength(4)
  })

  it('re-uploads a file the drive received damaged', async () => {
    renderWithAgent(<Live />, 'held')
    await screen.findAllByRole('listitem')
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Re-upload' }))
    })
    await waitFor(() => expect(screen.getAllByRole('listitem')).toHaveLength(3))
    expect(screen.queryByText('Damaged on the way')).toBeNull()
  })

  it('has a helpful empty state', async () => {
    renderWithAgent(<Live />, 'empty')
    expect(await screen.findByText('Nothing is held.')).toBeTruthy()
  })
})
