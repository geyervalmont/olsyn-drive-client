import { useCallback, useEffect, useState } from 'react'
import { Onboarding, needsDriver, type OnboardingStep } from './screens/Onboarding'
import { MainWindow } from './screens/main/MainWindow'
import { Popover } from './screens/popover/Popover'
import { useAgentContext, useAgentView } from './state/context'
import { LetterMark } from './ui/LetterMark'
import { Spinner } from './ui/Progress'
import { useToast } from './ui/Toasts'
import { Button } from './ui/controls'

export type Surface = 'main' | 'popover'

export function App({ surface }: { surface: Surface }) {
  const view = useAgentView()
  const { shell } = useAgentContext()
  const [route, setRoute] = useState(() => (typeof location !== 'undefined' && location.hash.slice(1)) || 'transfers/active')
  const navigate = useCallback((next: string) => {
    setRoute(next)
    if (typeof history !== 'undefined') history.replaceState(null, '', '#' + next)
  }, [])
  const [onboarding, setOnboarding] = useState<OnboardingStep | null>(null)
  // Set when the person closes a setup that is not finished (driver waits
  // for a restart of Windows), so it is not put straight back.
  const [setupDeferred, setSetupDeferred] = useState(false)
  const state = view.state

  useEffect(() => shell.onRoute(navigate), [shell, navigate])

  // First run, or signed out: the setup flow takes the main window.
  useEffect(() => {
    if (surface !== 'main' || !state) return
    if (onboarding !== null) return
    if (!state.account) setOnboarding(state.signin?.state === 'waiting' ? 'signin' : 'welcome')
    // Signed in, but the drive letter was never chosen (the app was closed
    // during setup, or Windows was restarted for the driver): finish setup.
    else if (state.mount.letter_confirmed === false && !setupDeferred) setOnboarding(needsDriver(state.driver.state) ? 'driver' : 'letter')
  }, [surface, state, onboarding, setupDeferred])

  useEffect(() => {
    if (route !== 'signin') return
    if (!state) return
    if (!state.account) setOnboarding('signin')
    navigate('transfers/active')
  }, [route, state, navigate])

  if (surface === 'popover') {
    return (
      <>
        <Popover />
        <BrowserNotifications navigate={(r) => shell.showMain(r)} />
      </>
    )
  }
  if (!state) return <Starting />
  return (
    <>
      {onboarding ? <Onboarding
          key={onboarding}
          startAt={onboarding}
          onDone={() => {
            setSetupDeferred(true)
            setOnboarding(null)
          }}
        /> : <MainWindow route={route} navigate={navigate} />}
      <BrowserNotifications navigate={navigate} />
    </>
  )
}

function Starting() {
  const view = useAgentView()
  const { connection } = useAgentContext()
  const unreachable = view.stream === 'unreachable'
  return (
    <main className="starting" aria-busy={!unreachable}>
      <LetterMark letter="W:" tone={unreachable ? 'danger' : 'off'} progress={null} size={72} />
      <h1 className="display">{unreachable ? "Olsyn Drive isn't running." : 'Starting Olsyn Drive.'}</h1>
      <p>{unreachable ? 'Trying to start it again. If this stays, restart the PC.' : 'This takes a few seconds.'}</p>
      {unreachable ? (
        <Button variant="secondary" icon="refresh" onClick={() => connection.retryNow()}>
          Try again
        </Button>
      ) : (
        <Spinner size={18} label="Loading" />
      )}
    </main>
  )
}

/** The desktop shell turns notifications into Windows toasts; in a browser they show in the window. */
function BrowserNotifications({ navigate }: { navigate: (route: string) => void }) {
  const { connection, shell, client } = useAgentContext()
  const toast = useToast()
  useEffect(() => {
    if (shell.kind !== 'browser') return
    return connection.onEvent((e) => {
      if (e.event !== 'notification') return
      const n = e.data
      const open = n.action?.open
      toast({
        icon: n.kind === 'held' ? 'hold' : n.kind === 'conflict' ? 'copy' : n.kind === 'signed_out' ? 'key' : n.kind === 'update_applied' ? 'sparkle' : 'check',
        title: n.title,
        body: n.body,
        action: open
          ? {
              label: n.action!.label,
              run: () => (open.target === 'ui' ? navigate(open.path ?? 'transfers') : void client.open(open).catch(() => undefined)),
            }
          : undefined,
      })
    })
  }, [connection, shell, client, toast, navigate])
  return null
}
