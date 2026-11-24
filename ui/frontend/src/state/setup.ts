// Which setup step (if any) the main window shows. It is worked out from the
// agent's state every time that state changes, so the window follows what
// happens elsewhere: a sign-in from the command line, a letter chosen in
// another window, a PC removed in the web app, an agent that restarts, or an
// app that started before its agent. The window remembers only what the
// person did in it (asked to sign in, closed a step) and whether the short
// "Done" step is owed; none of that can hide a signed-in state.

import type { DriverState, State } from '../api/types'

export type SetupStep = 'welcome' | 'signin' | 'driver' | 'letter' | 'done'

export const needsDriver = (state: DriverState | string | undefined) =>
  state === 'missing' || state === 'outdated' || state === 'installing' || state === 'restart_needed'

export interface SetupMemory {
  /** The sign-in step is open in this window (the person chose Sign in, or a code was shown here): it stays, with its outcome, until they go back. */
  signinOpen: boolean
  /** The short "Done" step is owed: this is the PC's first setup (first_run), or the person went through the driver or letter step here. */
  doneOwed: boolean
  /** The driver step was closed while the driver was in this state ("sign out of Windows first", or "not now"): it stays closed until that state changes. */
  driverDeferredFor: DriverState | null
  /** The driver step was shown in this window (the step list keeps it once it is done). */
  driverShown: boolean
  /** The last sign-in code seen waiting: a new one opens the sign-in step, the same one does not reopen it after Cancel. */
  lastCode: string | null
}

export const initialSetupMemory: SetupMemory = { signinOpen: false, doneOwed: false, driverDeferredFor: null, driverShown: false, lastCode: null }

/** The step to show, or null for the main window. */
export function setupStep(state: State, mem: SetupMemory): SetupStep | null {
  if (!state.account) {
    // A code is waiting for the browser, whoever asked for it (this window, another, the command line).
    if (state.signin?.state === 'waiting' || state.connection === 'linking') return 'signin'
    return mem.signinOpen ? 'signin' : 'welcome'
  }
  const driverDeferred = mem.driverDeferredFor !== null && mem.driverDeferredFor === state.driver.state
  if (needsDriver(state.driver.state) && !driverDeferred) return 'driver'
  // The agent does not connect the drive before the letter is chosen.
  if (state.mount.letter_confirmed === false) return driverDeferred ? null : 'letter'
  return mem.doneOwed ? 'done' : null
}

/**
 * What the window remembers after showing `step` for `state`. Returns
 * `mem` itself when nothing changes, so it can be used in an effect.
 */
export function rememberSetup(mem: SetupMemory, state: State, step: SetupStep | null): SetupMemory {
  let next = mem
  const set = (patch: Partial<SetupMemory>) => {
    for (const [k, v] of Object.entries(patch) as [keyof SetupMemory, never][]) {
      if (next[k] !== v) next = { ...next, [k]: v }
    }
  }
  const waiting = state.signin?.state === 'waiting' ? state.signin.user_code : null
  if (state.account) set({ signinOpen: false })
  else if (waiting !== null && waiting !== mem.lastCode) set({ signinOpen: true, lastCode: waiting })
  if (state.first_run === true || step === 'driver' || step === 'letter') set({ doneOwed: true })
  if (step === 'driver') set({ driverShown: true })
  if (mem.driverDeferredFor !== null && state.driver.state !== mem.driverDeferredFor) set({ driverDeferredFor: null })
  return next
}
