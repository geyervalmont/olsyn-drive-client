// The window follows the agent, whoever changes it: the command line, another
// window, the drive (a PC removed in the web app), an agent restart, or an app
// that started before its agent. Each test drives the mock agent from outside
// the window and checks the window through the event stream alone.

import { act, fireEvent, screen, waitFor } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { App } from '../App'
import type { State } from '../api/types'
import { initialSetupMemory, rememberSetup, setupStep } from '../state/setup'
import { state as fixtureState } from '../test/fixtures'
import { renderWithAgent } from '../test/harness'

const welcome = () => screen.findByRole('heading', { name: 'Your Olsyn drive, as a drive on this PC.' })
const mainWindow = () => screen.findByRole('navigation', { name: 'Olsyn Drive' })
/** What another program (the command line, another window) asks of the agent. */
const elsewhere = (agent: { handle: (m: string, p: string, b?: unknown) => Promise<unknown> }, method: string, path: string, body?: unknown) =>
  act(async () => {
    await agent.handle(method, path, body)
  })

describe('setupStep', () => {
  const signedOut = fixtureState({ account: null, connection: 'signed_out' })
  it('signed in and mounted is the main window, whatever first_run says', () => {
    expect(setupStep(fixtureState({ first_run: true }), initialSetupMemory)).toBeNull()
    expect(setupStep(fixtureState({ first_run: false }), initialSetupMemory)).toBeNull()
  })
  it('signed out is the welcome step; a waiting code is the sign-in step, whoever asked for it', () => {
    expect(setupStep(signedOut, initialSetupMemory)).toBe('welcome')
    const linking: State = { ...signedOut, connection: 'linking', signin: { state: 'waiting', user_code: 'BCDF-GHJK', verification_url: 'https://drive.olsyn.com/link', expires_at: '2026-11-24T01:00:00Z', error: null } }
    expect(setupStep(linking, initialSetupMemory)).toBe('signin')
  })
  it('a missing driver is the driver step; a letter not chosen yet is the letter step', () => {
    expect(setupStep(fixtureState({ driver: { state: 'missing', name: 'winfsp', version: null, min_version: '2.0' } }), initialSetupMemory)).toBe('driver')
    expect(setupStep(fixtureState({ mount: { state: 'unmounted', letter: 'W:', path: null, error: null, letter_confirmed: false } }), initialSetupMemory)).toBe('letter')
  })
  it('first_run only decides whether the Done step is owed', () => {
    const mem = rememberSetup(initialSetupMemory, fixtureState({ account: null, connection: 'signed_out', first_run: true }), 'welcome')
    expect(mem.doneOwed).toBe(true)
    expect(setupStep(fixtureState(), mem)).toBe('done')
    expect(setupStep(fixtureState({ account: null, connection: 'signed_out' }), mem)).toBe('welcome')
  })
  it('remembers nothing new when nothing changed', () => {
    expect(rememberSetup(initialSetupMemory, fixtureState(), null)).toBe(initialSetupMemory)
  })
})

describe('the window follows the agent', () => {
  it('signed in from the command line: Welcome becomes the code step, then the main window', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'signed-out')
    expect(await welcome()).toBeTruthy()

    // olsyn-drive login --no-browser
    await elsewhere(agent, 'POST', '/v1/signin', { open_browser: false })
    expect(await screen.findByRole('heading', { name: 'Approve this PC in your browser.' })).toBeTruthy()
    const code = agent.state().signin!.user_code
    await waitFor(() => expect(document.querySelector('.code')?.textContent).toBe(code))

    // Approved in the browser; the agent connects the letter chosen before.
    await act(async () => agent.tick(8))
    await act(async () => agent.tick(2))
    expect(await mainWindow()).toBeTruthy()
    expect(agent.state().mount.state).toBe('mounted')
    expect(screen.queryByRole('button', { name: 'Sign in' })).toBeNull()
  })

  it('signed in and approved elsewhere while the window shows Welcome, before any code was seen here', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'signed-out')
    expect(await welcome()).toBeTruthy()
    // The agent restarts already signed in and mounted (the window never saw the code).
    act(() => agent.restart((a) => (a.signIn(), a.mountNow())))
    await act(async () => agent.tick(2))
    expect(await mainWindow()).toBeTruthy()
    expect(screen.queryByRole('heading', { name: 'Your Olsyn drive, as a drive on this PC.' })).toBeNull()
  })

  it('a new PC signed in from the command line goes on to the letter step, and the Done step follows', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'signin')
    expect(await welcome()).toBeTruthy()
    await elsewhere(agent, 'POST', '/v1/signin', { open_browser: false })
    expect(await screen.findByText('Your code')).toBeTruthy()
    await act(async () => agent.tick(8))
    expect(await screen.findByRole('heading', { name: 'Choose a letter for the drive.' })).toBeTruthy()

    // The letter is chosen in another window (or with the command line).
    await elsewhere(agent, 'POST', '/v1/mount', { letter: 'W:' })
    expect(await screen.findByRole('heading', { name: 'Connecting W:.' })).toBeTruthy()
    await act(async () => agent.tick(2))
    expect(await screen.findByRole('heading', { name: 'Your drive is W:' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(await mainWindow()).toBeTruthy()
  })

  it('a sign-in started elsewhere that runs out keeps its outcome on screen, and Back returns to Welcome', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'link-expired')
    expect(await welcome()).toBeTruthy()
    await elsewhere(agent, 'POST', '/v1/signin', { open_browser: false })
    expect(await screen.findByText('Your code')).toBeTruthy()
    await act(async () => agent.tick(25))
    expect(await screen.findByText(/The code ran out before it was approved/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Back' }))
    expect(await welcome()).toBeTruthy()
  })

  it('a sign-in cancelled from the command line leaves the window on Welcome', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'signed-out')
    expect(await welcome()).toBeTruthy()
    await elsewhere(agent, 'POST', '/v1/signin', { open_browser: false })
    expect(await screen.findByText('Your code')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(await welcome()).toBeTruthy()
    // The same code does not reopen the step.
    await act(async () => agent.tick(1))
    expect(screen.queryByText('Your code')).toBeNull()
  })

  it('the PC removed in the web app: the main window becomes Welcome, saying why', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'empty')
    expect(await mainWindow()).toBeTruthy()
    act(() => agent.revoke())
    expect(await welcome()).toBeTruthy()
    expect(screen.getByText('This PC was removed from your account in the web app.')).toBeTruthy()
  })

  it('signed out from the command line: the main window becomes Welcome; signed in again, it comes back without the Done step', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'empty')
    expect(await mainWindow()).toBeTruthy()
    await elsewhere(agent, 'POST', '/v1/signout', {})
    expect(await welcome()).toBeTruthy()
    await elsewhere(agent, 'POST', '/v1/signin', { open_browser: false })
    await act(async () => agent.tick(8))
    await act(async () => agent.tick(2))
    expect(await mainWindow()).toBeTruthy()
    expect(screen.queryByRole('heading', { name: /Your drive is/ })).toBeNull()
  })

  it('an agent that restarts signed out takes the main window back to Welcome, and back again', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'empty')
    expect(await mainWindow()).toBeTruthy()
    act(() => agent.restart((a) => a.signOut()))
    await act(async () => agent.tick(2))
    expect(await welcome()).toBeTruthy()
    act(() => agent.restart((a) => (a.signIn(), a.mountNow())))
    await act(async () => agent.tick(2))
    await waitFor(() => expect(screen.getByRole('navigation', { name: 'Olsyn Drive' })).toBeTruthy(), { timeout: 5000 })
  })

  it('an app that starts before its agent waits, then shows the signed-in window', async () => {
    renderWithAgent(<App surface="main" />, 'unreachable')
    expect(await screen.findByText(/Starting Olsyn Drive|isn't running/)).toBeTruthy()
    await waitFor(() => expect(screen.getByRole('navigation', { name: 'Olsyn Drive' })).toBeTruthy(), { timeout: 8000 })
    expect(screen.queryByRole('button', { name: 'Sign in' })).toBeNull()
  }, 10_000)

  it('a driver removed while signed in shows the driver step; added elsewhere, the drive connects and the main window returns', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'driver-missing')
    expect(await screen.findByRole('heading', { name: 'One Windows component to add.' })).toBeTruthy()
    await elsewhere(agent, 'POST', '/v1/driver/install')
    await act(async () => agent.tick(5))
    await act(async () => agent.tick(2))
    expect(await screen.findByRole('heading', { name: 'Your drive is W:' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Close' }))
    expect(await mainWindow()).toBeTruthy()
  })

  it('the window reads the state again when it comes to the front', async () => {
    const { agent, connection } = renderWithAgent(<App surface="main" />, 'signed-out')
    expect(await welcome()).toBeTruthy()
    // The stream goes quiet: nothing reaches this window.
    const store = connection.store
    const dispatch = store.dispatch
    store.dispatch = (a) => (a.type === 'event' ? undefined : dispatch(a))
    await elsewhere(agent, 'POST', '/v1/signin', { open_browser: false })
    await act(async () => agent.tick(8))
    await act(async () => agent.tick(2))
    expect(screen.queryByRole('navigation', { name: 'Olsyn Drive' })).toBeNull()
    await act(async () => {
      window.dispatchEvent(new Event('focus'))
    })
    expect(await mainWindow()).toBeTruthy()
  })
})

describe('the popover follows the agent', () => {
  it('signed out, linking from the command line, then signed in', async () => {
    const { agent } = renderWithAgent(<App surface="popover" />, 'signed-out')
    expect(await screen.findByRole('button', { name: 'Sign in' })).toBeTruthy()
    await elsewhere(agent, 'POST', '/v1/signin', { open_browser: false })
    expect(await screen.findByRole('button', { name: 'Show the code' })).toBeTruthy()
    expect(screen.getByText('Waiting for your browser.')).toBeTruthy()
    await act(async () => agent.tick(8))
    await act(async () => agent.tick(2))
    await waitFor(() => expect(screen.queryByRole('button', { name: /Sign in|Show the code/ })).toBeNull())
    expect(screen.getByRole('region', { name: 'Recently finished' })).toBeTruthy()
    act(() => agent.revoke())
    expect((await screen.findAllByText('Signed out by the drive.')).length).toBeGreaterThan(0)
    expect(screen.getAllByRole('button', { name: 'Sign in' })).toHaveLength(1)
  })
})
