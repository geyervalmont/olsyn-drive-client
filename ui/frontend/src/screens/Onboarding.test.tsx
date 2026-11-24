import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { App } from '../App'
import { renderWithAgent } from '../test/harness'

describe('first-run onboarding', () => {
  it('goes welcome → sign in with a code → driver → letter → done, then opens the drive', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'first-run')
    expect(await screen.findByRole('heading', { name: 'Your Olsyn drive, as a drive on this PC.' })).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Sign in' }))
    expect(await screen.findByRole('heading', { name: 'Approve this PC in your browser.' })).toBeTruthy()
    expect(await screen.findByText('Your code')).toBeTruthy()
    expect(screen.getByText(/Waiting for your browser/)).toBeTruthy()
    expect(screen.getByText(/The code works for/).textContent).toMatch(/10:00|9:5\d/)
    expect(screen.getByRole('list', { name: 'Setup steps' })).toBeTruthy()

    // The browser approves.
    await act(async () => agent.tick(8))
    expect(await screen.findByRole('heading', { name: 'One Windows component to add.' })).toBeTruthy()
    expect(screen.getByText(/Windows will ask whether to allow changes/)).toBeTruthy()

    fireEvent.click(screen.getByRole('button', { name: 'Add WinFsp' }))
    const progress = await screen.findByRole('list', { name: 'Adding WinFsp' })
    expect(within(progress).getByText('Checking the installer')).toBeTruthy()
    await act(async () => agent.tick(1))
    await waitFor(() => expect(progress.querySelector('[aria-current="step"]')?.textContent).toMatch(/choose Yes in the Windows prompt/))
    await act(async () => agent.tick(3))

    expect(await screen.findByRole('heading', { name: 'Choose a letter for the drive.' })).toBeTruthy()
    const group = await screen.findByRole('radiogroup', { name: 'Drive letter' })
    const selected = within(group).getByRole('radio', { checked: true })
    expect(selected.textContent).toBe('W:')
    // Arrow keys move the choice; only free letters are offered.
    fireEvent.keyDown(group, { key: 'ArrowRight' })
    expect(within(group).getByRole('radio', { checked: true }).textContent).toBe('X:')
    fireEvent.keyDown(group, { key: 'ArrowLeft' })

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Connect W:' }))
    })
    await act(async () => agent.tick(2))
    expect(await screen.findByRole('heading', { name: 'Your drive is W:' })).toBeTruthy()

    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Open W:' }))
    })
    // Onboarding is over: the main window shows.
    await waitFor(() => expect(screen.getByRole('navigation', { name: 'Olsyn Drive' })).toBeTruthy())
  })

  it('says to sign out of Windows when the driver needs it, and finishes setup afterwards', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'driver-restart')
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in' }))
    await screen.findByText('Your code')
    await act(async () => agent.tick(8))
    fireEvent.click(await screen.findByRole('button', { name: 'Add WinFsp' }))
    await act(async () => agent.tick(5))
    expect(await screen.findByRole('heading', { name: 'Almost there.' })).toBeTruthy()
    expect(screen.getByText(/sign out and back in/)).toBeTruthy()
    expect(agent.state().mount.state).toBe('unmounted')
    fireEvent.click(screen.getByRole('button', { name: 'OK' }))
    // Closed for now: the main window shows, and says what is left to do.
    await waitFor(() => expect(screen.getByRole('navigation', { name: 'Olsyn Drive' })).toBeTruthy())
  })

  it('links to the WinFsp download page when the download does not work, and checks again', async () => {
    const { agent, shell } = renderWithAgent(<App surface="main" />, 'driver-offline')
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in' }))
    await screen.findByText('Your code')
    await act(async () => agent.tick(8))
    fireEvent.click(await screen.findByRole('button', { name: 'Add WinFsp' }))
    const progress = await screen.findByRole('list', { name: 'Adding WinFsp' })
    expect(within(progress).getByText(/Downloading WinFsp/)).toBeTruthy()
    await act(async () => agent.tick(1))
    expect(await screen.findByRole('heading', { name: 'WinFsp could not be downloaded.' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Open the WinFsp download page' }))
    expect(shell.opened).toContain('https://winfsp.dev/rel/')
    // The person installs WinFsp by hand, then checks again: setup moves on to the letter.
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Check again' }))
    })
    await act(async () => agent.tick(1))
    expect(await screen.findByRole('heading', { name: 'Choose a letter for the drive.' })).toBeTruthy()
  })

  it('offers to try again when the Windows prompt is declined', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'driver-declined')
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in' }))
    await screen.findByText('Your code')
    await act(async () => agent.tick(8))
    fireEvent.click(await screen.findByRole('button', { name: 'Add WinFsp' }))
    await act(async () => agent.tick(3))
    expect(await screen.findByText(/the prompt was declined/)).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }))
    await act(async () => agent.tick(5))
    expect(await screen.findByRole('heading', { name: 'Choose a letter for the drive.' })).toBeTruthy()
  })

  it('does not connect before the letter is chosen, and picks the next free one when W: is taken', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'letter-taken')
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in' }))
    await screen.findByText('Your code')
    await act(async () => agent.tick(8))
    expect(await screen.findByRole('heading', { name: 'Choose a letter for the drive.' })).toBeTruthy()
    expect(agent.state().mount.state).toBe('unmounted')
    expect(agent.state().mount.letter_confirmed).toBe(false)
    const group = await screen.findByRole('radiogroup', { name: 'Drive letter' })
    expect(within(group).getByRole('radio', { checked: true }).textContent).toBe('X:')
    expect(within(group).queryByText('W:')).toBeNull()
    expect(within(group).queryByText('O:')).toBeNull() // a disconnected network mapping
    expect(screen.getByRole('status').textContent).toMatch(/W: is already used on this PC by a network drive, so X: is selected/)
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Connect X:' }))
    })
    await act(async () => agent.tick(2))
    expect(await screen.findByRole('heading', { name: 'Your drive is X:' })).toBeTruthy()
    expect(agent.state().mount.letter_confirmed).toBe(true)
  })

  it('offers a new code when the old one runs out', async () => {
    const { agent } = renderWithAgent(<App surface="main" />, 'link-expired')
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in' }))
    await screen.findByText('Your code')
    await act(async () => agent.tick(25))
    expect(await screen.findByText(/The code ran out before it was approved/)).toBeTruthy()
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Get a new code' }))
    })
    expect(await screen.findByText('Your code')).toBeTruthy()
  })

  it('cancel goes back to the welcome step', async () => {
    renderWithAgent(<App surface="main" />, 'signin')
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in' }))
    await screen.findByText('Your code')
    await act(async () => {
      fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    })
    expect(await screen.findByRole('heading', { name: 'Your Olsyn drive, as a drive on this PC.' })).toBeTruthy()
  })
})

describe('sign-in when Olsyn Drive answers late', () => {
  it('keeps trying, clears the problem once it answers, and can open the page again', async () => {
    const { agent, shell } = renderWithAgent(<App surface="main" />, 'first-run')
    const handle = agent.handle.bind(agent)
    let refused = 0
    agent.handle = (method: string, path: string, body?: unknown) => {
      if (method === 'POST' && path === '/v1/signin' && refused < 2) {
        refused++
        throw new Error('not running yet')
      }
      return handle(method, path, body)
    }
    fireEvent.click(await screen.findByRole('button', { name: 'Sign in' }))
    expect(await screen.findByText(/isn't answering on this PC yet. Trying again/)).toBeTruthy()
    // 0.5 s then 1 s later the third attempt gets a code and the problem goes away.
    await waitFor(() => expect(screen.getByText('Your code')).toBeTruthy(), { timeout: 5000 })
    expect(screen.queryByText(/isn't answering/)).toBeNull()
    expect(refused).toBe(2)

    fireEvent.click(screen.getByRole('button', { name: 'Open the page again' }))
    expect(shell.opened).toHaveLength(1)
    expect(shell.opened[0]).toMatch(/^https:\/\//)
  })
})
