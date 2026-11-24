// Every way to a screen (olsyn-drive open <screen>, olsyn-drive://open/<screen>,
// a notification) reaches the page as a route from the shell.

import { act, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { App } from '../App'
import { renderWithAgent, testShell } from '../test/harness'

function routingShell() {
  const shell = testShell()
  const listeners = new Set<(route: string) => void>()
  shell.onRoute = (l) => {
    listeners.add(l)
    return () => listeners.delete(l)
  }
  return { shell, go: (route: string) => act(() => listeners.forEach((l) => l(route))) }
}

describe('screens by name', () => {
  it('shows each screen the shell names, with the transfers filter', async () => {
    const { shell, go } = routingShell()
    renderWithAgent(<App surface="main" />, 'held', shell)
    await screen.findByRole('navigation', { name: 'Olsyn Drive' })
    const heading = (name: string) => screen.findByRole('heading', { level: 1, name })

    for (const [route, title] of [['activity', 'Activity'], ['settings', 'Settings'], ['account', 'Account'], ['about', 'About']] as const) {
      go(route)
      expect(await heading(title)).toBeTruthy()
    }
    for (const [filter, label] of [['active', 'Sending'], ['waiting', 'Waiting'], ['held', 'Held'], ['done', 'Done'], ['skipped', 'Already there']] as const) {
      go('transfers/' + filter)
      expect(await heading('Transfers')).toBeTruthy()
      const tab = await screen.findByRole('tab', { selected: true })
      expect(tab.textContent).toContain(label)
    }
  })
})
