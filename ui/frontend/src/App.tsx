import { useCallback, useEffect, useState } from 'react'
import { Onboarding } from './screens/Onboarding'
import { MainWindow } from './screens/main/MainWindow'
import { Popover } from './screens/popover/Popover'
import { useAgentContext, useAgentView } from './state/context'
import { initialSetupMemory, rememberSetup, setupStep, type SetupMemory } from './state/setup'
import { LetterMark } from './ui/LetterMark'
import { Spinner } from './ui/Progress'
import { useToast } from './ui/Toasts'
import { Button } from './ui/controls'

export type Surface = 'main' | 'popover'

export function App({ surface }: { surface: Surface }) {
  const view = useAgentView()
  const { shell, connection } = useAgentContext()
  const [route, setRoute] = useState(() => (typeof location !== 'undefined' && location.hash.slice(1)) || 'transfers/active')
  const navigate = useCallback((next: string) => {
    setRoute(next)
    if (typeof history !== 'undefined') history.replaceState(null, '', '#' + next)
  }, [])
  const [setup, setSetup] = useState<SetupMemory>(initialSetupMemory)
  const state = view.state

  useEffect(() => shell.onRoute(navigate), [shell, navigate])
  useRefreshOnActivate(connection.refresh.bind(connection))

  // The setup step is worked out from the agent's state on every render, so
  // the window always shows what the agent says (state/setup.ts).
  const step = surface === 'main' && state ? setupStep(state, setup) : null
  useEffect(() => {
    if (state) setSetup((m) => rememberSetup(m, state, step))
  }, [state, step])

  // "signin" (the popover's Sign in, a "signed out" notification) opens the
  // sign-in step when signed out; signed in, it is just the main window.
  useEffect(() => {
    if (route !== 'signin' || !state) return
    if (!state.account) setSetup((m) => (m.signinOpen ? m : { ...m, signinOpen: true }))
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
      {step ? (
        <Onboarding
          step={step}
          driverShown={setup.driverShown}
          onSignIn={() => setSetup((m) => ({ ...m, signinOpen: true }))}
          onBack={() => setSetup((m) => ({ ...m, signinOpen: false }))}
          onDeferDriver={() => setSetup((m) => ({ ...m, driverDeferredFor: state.driver.state }))}
          onDone={() => setSetup((m) => ({ ...m, doneOwed: false }))}
        />
      ) : (
        <MainWindow route={route} navigate={navigate} />
      )}
      <BrowserNotifications navigate={navigate} />
    </>
  )
}

/** Reads the agent's state again whenever the window comes to the front. */
function useRefreshOnActivate(refresh: () => void) {
  useEffect(() => {
    if (typeof window === 'undefined') return
    const onVisible = () => {
      if (document.visibilityState === 'visible') refresh()
    }
    window.addEventListener('focus', refresh)
    document.addEventListener('visibilitychange', onVisible)
    return () => {
      window.removeEventListener('focus', refresh)
      document.removeEventListener('visibilitychange', onVisible)
    }
    // refresh is bound once per connection
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])
}

function Starting() {
  const view = useAgentView()
  const { connection, shell } = useAgentContext()
  const unreachable = view.stream === 'unreachable'
  const blocked = view.blocked
  if (blocked) {
    return (
      <main className="starting">
        <LetterMark letter="W:" tone="danger" progress={null} size={72} />
        <h1 className="display">{blocked.title}</h1>
        <p>{blocked.detail} They stay on this PC and carry on uploading once Olsyn Drive is allowed and installed again.</p>
        <div className="starting-actions">
          <Button variant="primary" onClick={() => shell.openUrl(blocked.helpUrl)}>
            What to do
          </Button>
          <Button variant="secondary" icon="refresh" onClick={() => connection.retryNow()}>
            Try again
          </Button>
        </div>
      </main>
    )
  }
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
