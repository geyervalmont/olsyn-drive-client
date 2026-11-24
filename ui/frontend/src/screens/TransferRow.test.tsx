// Why a file is not plain "done" is written out on its row, with what can be
// done about it, without hovering: a date the drive could not keep, a conflict
// copy, a held file.

import { fireEvent, render, screen, within } from '@testing-library/react'
import type { ReactNode } from 'react'
import { describe, expect, it } from 'vitest'
import { AgentClient } from '../api/client'
import { MockAgent } from '../mock/agent'
import { MockTransport } from '../mock/transport'
import { AgentProvider } from '../state/context'
import { AgentConnection } from '../state/connection'
import { createStore } from '../state/store'
import { transfer } from '../test/fixtures'
import { testShell } from '../test/harness'
import { ToastProvider } from '../ui/Toasts'
import { HeldList } from './main/HeldList'
import { TransferRow } from './main/TransferRow'

function renderRows(ui: ReactNode) {
  const agent = new MockAgent({ scenario: 'empty' })
  const client = new AgentClient(new MockTransport(agent, 0))
  const opened: unknown[] = []
  const open = client.open.bind(client)
  client.open = (req) => {
    opened.push(req)
    return open(req)
  }
  render(
    <AgentProvider value={{ client, connection: new AgentConnection(client, createStore()), shell: testShell() }}>
      <ToastProvider>{ui}</ToastProvider>
    </AgentProvider>,
  )
  return { opened }
}

const doneAt = { state: 'done' as const, bytes_done: 1000, finished_at: '2026-11-24T00:00:00.000Z' }

describe('reasons on transfer rows', () => {
  it('says plainly when the drive could not keep a file date', () => {
    renderRows(<TransferRow t={transfer('survey', { ...doneAt, note: 'date_not_kept' })} now={Date.parse('2026-11-24T00:05:00Z')} />)
    expect(screen.getByText("Uploaded. The file's date could not be kept.")).toBeTruthy()
  })

  it('a conflict says what happened, names the copy and offers to show both', () => {
    const { opened } = renderRows(
      <TransferRow t={transfer('plan', { ...doneAt, conflict_copy: '/Projects/plan (conflict from ADA-PC, 24 Nov 2026).jpg' })} now={Date.parse('2026-11-24T00:05:00Z')} />,
    )
    expect(screen.getByText('Changed on the drive while you saved. Yours is kept beside it as plan (conflict from ADA-PC, 24 Nov 2026).jpg.')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Show both' }))
    expect(opened).toEqual([{ target: 'web', path: '/Projects/plan (conflict from ADA-PC, 24 Nov 2026).jpg' }])
  })

  it('an ordinary done row shows its folder, no warning', () => {
    renderRows(<TransferRow t={transfer('photo', doneAt)} now={Date.parse('2026-11-24T00:05:00Z')} />)
    expect(screen.queryByText(/could not be kept|Changed on the drive/)).toBeNull()
  })

  it('a held file shows its reason and every action at once', () => {
    const held = transfer('model', {
      state: 'held',
      held: { code: 'sealed', message: "This folder is sealed, so new files can't be added.", since: '2026-11-24T00:00:00.000Z', actions: ['save_copy', 'discard'] },
    })
    renderRows(<HeldList items={[held]} />)
    const row = screen.getByRole('article', { name: /model\.jpg: Sealed folder/ })
    expect(within(row).getByText("This folder is sealed, so new files can't be added.")).toBeTruthy()
    expect(within(row).getByRole('button', { name: 'Save a copy…' })).toBeTruthy()
    expect(within(row).getByRole('button', { name: 'Discard' })).toBeTruthy()
  })
})
