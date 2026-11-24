// First run: welcome → sign in (code + waiting for the browser) → driver (only
// when missing) → drive letter → done, with the drive opened in Explorer.

import { useEffect, useRef, useState, type KeyboardEvent } from 'react'
import type { DriverProgress, SettingsResponse } from '../api/types'
import { formatCountdown, secondsUntil } from '../lib/format'
import { useNow } from '../lib/useNow'
import { errorText, useAction, useAgentContext, useAgentView } from '../state/context'
import { cacheExplainer } from '../state/derive'
import { Icon } from '../ui/Icon'
import { LetterMark } from '../ui/LetterMark'
import { Spinner } from '../ui/Progress'
import { Button, InlineError } from '../ui/controls'

export type OnboardingStep = 'welcome' | 'signin' | 'driver' | 'letter' | 'done'

export const needsDriver = (state: string | undefined) =>
  state === 'missing' || state === 'outdated' || state === 'installing' || state === 'restart_needed'

export function Onboarding({ onDone, startAt = 'welcome' }: { onDone: () => void; startAt?: OnboardingStep }) {
  const view = useAgentView()
  const state = view.state
  const [step, setStep] = useState<OnboardingStep>(startAt)
  const [driverStepShown, setDriverStepShown] = useState(false)

  // Signed in: move on by itself. The drive is not connected before the
  // person has chosen its letter (mount.letter_confirmed).
  const letterChosen = state?.mount.letter_confirmed !== false && state?.mount.state === 'mounted'
  useEffect(() => {
    if (step !== 'signin' || !state?.account) return
    if (needsDriver(state.driver.state)) {
      setDriverStepShown(true)
      setStep('driver')
    } else if (letterChosen) setStep('done')
    else setStep('letter')
  }, [step, state?.account, state?.driver.state, letterChosen])

  const steps: { key: OnboardingStep; label: string }[] = [
    { key: 'signin', label: 'Sign in' },
    ...(driverStepShown || needsDriver(state?.driver.state) ? [{ key: 'driver' as const, label: 'Windows component' }] : []),
    { key: 'letter', label: 'Drive letter' },
    { key: 'done', label: 'Done' },
  ]
  const index = steps.findIndex((s) => s.key === step)

  return (
    <main className="onboarding" aria-labelledby="onboarding-title">
      <div className="onboarding-card">
        {step !== 'welcome' && (
          <ol className="steps" aria-label="Setup steps">
            {steps.map((s, i) => (
              <li key={s.key} className={i < index ? 'is-done' : i === index ? 'is-current' : ''} aria-current={i === index ? 'step' : undefined}>
                <span className="steps-dot" aria-hidden="true">
                  {i < index ? <Icon name="check" size={12} /> : i + 1}
                </span>
                <span className="steps-label">{s.label}</span>
              </li>
            ))}
          </ol>
        )}
        {step === 'welcome' && <Welcome onNext={() => setStep('signin')} />}
        {step === 'signin' && <SignInStep onBack={() => setStep('welcome')} />}
        {step === 'driver' && <DriverStep onNext={() => (letterChosen ? setStep('done') : setStep('letter'))} onLater={onDone} />}
        {step === 'letter' && <LetterStep onNext={() => setStep('done')} />}
        {step === 'done' && <DoneStep onDone={onDone} />}
      </div>
    </main>
  )
}

function Welcome({ onNext }: { onNext: () => void }) {
  const view = useAgentView()
  const server = view.state?.account?.server ?? 'https://drive.olsyn.com'
  const reason = view.state?.reason
  return (
    <section className="onb-step onb-welcome">
      <LetterMark letter="W:" tone="ok" progress={null} size={88} />
      <h1 id="onboarding-title" className="onb-title display">
        Your Olsyn drive, as a drive on this PC.
      </h1>
      <p className="onb-lead">Open, save and drag files on W: like any other drive. They upload to the Olsyn drive by themselves, and everyone on the project sees them.</p>
      {reason && reason.code !== 'signed_out' && <p className="onb-note">{reason.message}</p>}
      <div className="onb-actions">
        <Button variant="primary" size="l" onClick={onNext} data-autofocus>
          Sign in
        </Button>
      </div>
      <p className="onb-fine">You sign in with your browser on {server.replace(/^https?:\/\//, '')}.</p>
    </section>
  )
}

/** While Olsyn Drive is not answering, the sign-in is tried again after these waits (ms), then every 5 s. */
export const SIGNIN_RETRY_MS = [500, 1000, 2000, 4000, 5000]
/** While the code waits for the browser, the state is also read this often, in case the event stream is down. */
export const SIGNIN_POLL_MS = 3000

function SignInStep({ onBack }: { onBack: () => void }) {
  const view = useAgentView()
  const { client, shell, connection } = useAgentContext()
  const signin = view.state?.signin ?? null
  const { pending, error, run } = useAction()
  const started = useRef(false)
  const retries = useRef(0)
  const waiting = signin?.state === 'waiting'
  const now = useNow(1000, waiting)

  const start = () =>
    run('start', async () => {
      const s = await client.signIn(true)
      retries.current = 0
      return s
    })

  // Olsyn Drive may still be starting: keep asking while this step is open,
  // and the problem line goes away by itself once it answers.
  const retrying = error?.key === 'start' && error.unreachable
  useEffect(() => {
    if (!retrying) return
    const wait = SIGNIN_RETRY_MS[Math.min(retries.current, SIGNIN_RETRY_MS.length - 1)]
    retries.current += 1
    const id = setTimeout(() => void start(), wait)
    return () => clearTimeout(id)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [error])

  // The agent polls the drive until the code is approved or runs out; the
  // page follows it through events, and reads the state as well in case the
  // event stream is down while the browser has the focus.
  useEffect(() => {
    if (!waiting) return
    const id = setInterval(() => {
      client
        .state()
        .then((state) => connection.store.dispatch({ type: 'state', state }))
        .catch(() => undefined)
    }, SIGNIN_POLL_MS)
    return () => clearInterval(id)
  }, [waiting, client, connection])

  useEffect(() => {
    if (started.current) return
    started.current = true
    if (view.state?.signin?.state !== 'waiting' && !view.state?.account) void start()
    // Start once when the step opens.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const left = waiting ? secondsUntil(signin.expires_at, now) : 0

  return (
    <section className="onb-step" aria-labelledby="onboarding-title">
      <h1 id="onboarding-title" className="onb-title display">
        {signin?.state === 'approved' || view.state?.account ? 'Signed in.' : 'Approve this PC in your browser.'}
      </h1>
      {(!signin || waiting || pending === 'start') && !error && (
        <>
          <p className="onb-lead">A browser window has opened. Check it shows the same code, then choose Approve.</p>
          <div className="code-card" aria-live="polite">
            {signin && waiting ? (
              <>
                <p className="code-label" id="code-label">
                  Your code
                </p>
                <p className="code" aria-labelledby="code-label">
                  {signin.user_code.split('').map((ch, i) => (
                    <span key={i} className={ch === '-' ? 'code-sep' : 'code-ch'}>
                      {ch}
                    </span>
                  ))}
                </p>
                <p className="code-wait">
                  <Spinner size={14} /> Waiting for your browser.{' '}
                  <span className="code-expiry">
                    The code works for <span className="tnum">{formatCountdown(left)}</span>.
                  </span>
                </p>
              </>
            ) : (
              <p className="code-wait">
                <Spinner size={14} /> Getting a code.
              </p>
            )}
          </div>
          {signin && waiting && (
            <p className="onb-fine">
              The browser didn't open, or you closed it?{' '}
              <button type="button" className="link-btn" onClick={() => shell.openUrl(signin.verification_url)}>
                Open the page again
              </button>
            </p>
          )}
          <div className="onb-actions">
            <Button
              variant="ghost"
              pending={pending === 'cancel'}
              onClick={() =>
                void run('cancel', async () => {
                  if (waiting) await client.cancelSignIn()
                  onBack()
                })
              }
            >
              Cancel
            </Button>
          </div>
        </>
      )}
      {signin && !waiting && signin.state !== 'approved' && (
        <SignInOutcome state={signin.state} message={signin.error?.message} onRetry={() => void start()} pending={pending === 'start'} onBack={onBack} />
      )}
      {error && (
        <div className="onb-problem">
          <InlineError
            message={
              error.code === 'offline'
                ? "Can't reach the drive. Check this PC is online, then try again."
                : retrying
                  ? "Olsyn Drive isn't answering on this PC yet. Trying again."
                  : error.message
            }
          />
          <div className="onb-actions">
            <Button variant="primary" onClick={() => void start()} pending={pending === 'start'}>
              Try again
            </Button>
            <Button variant="ghost" onClick={onBack}>
              Back
            </Button>
          </div>
        </div>
      )}
      {(signin?.state === 'approved' || view.state?.account) && (
        <p className="code-wait">
          <Spinner size={14} /> Getting your drive ready.
        </p>
      )}
    </section>
  )
}

function SignInOutcome({ state, message, onRetry, pending, onBack }: { state: string; message?: string; onRetry: () => void; pending: boolean; onBack: () => void }) {
  const text =
    state === 'expired'
      ? 'The code ran out before it was approved. Get a new one and try again.'
      : state === 'denied'
        ? 'The browser said no to this PC. If that was a mistake, try again.'
        : state === 'cancelled'
          ? 'Sign-in was cancelled.'
          : message || "Sign-in didn't finish. Try again."
  return (
    <div className="onb-problem">
      <InlineError message={text} />
      <div className="onb-actions">
        <Button variant="primary" onClick={onRetry} pending={pending}>
          {state === 'expired' ? 'Get a new code' : 'Try again'}
        </Button>
        <Button variant="ghost" onClick={onBack}>
          Back
        </Button>
      </div>
    </div>
  )
}

const DRIVER_STEPS: { key: DriverProgress; label: string }[] = [
  { key: 'preparing', label: 'Checking the installer' },
  { key: 'approval', label: 'Waiting for you to choose Yes in the Windows prompt' },
  { key: 'installing', label: 'Adding WinFsp' },
]

function DriverStep({ onNext, onLater }: { onNext: () => void; onLater: () => void }) {
  const view = useAgentView()
  const { client } = useAgentContext()
  const driver = view.state?.driver
  const { pending, error, run } = useAction()
  const installing = driver?.state === 'installing' || pending === 'install'

  useEffect(() => {
    if (driver?.state === 'ok' || driver?.state === 'not_needed') onNext()
  }, [driver?.state, onNext])

  if (driver?.state === 'restart_needed') {
    return (
      <section className="onb-step" aria-labelledby="onboarding-title">
        <span className="onb-icon" aria-hidden="true">
          <Icon name="check" size={26} />
        </span>
        <h1 id="onboarding-title" className="onb-title display">
          Almost there.
        </h1>
        <p className="onb-lead">WinFsp is added. Windows needs you to sign out and back in (or restart the PC) before the drive can appear.</p>
        <div className="onb-expect">
          <Icon name="info" size={17} />
          <p>Olsyn Drive starts again by itself when you sign back in, and asks for the drive letter then.</p>
        </div>
        <div className="onb-actions">
          <Button variant="primary" size="l" onClick={onLater} data-autofocus>
            OK
          </Button>
        </div>
      </section>
    )
  }

  const declined = driver?.error?.code === 'declined'
  const failed = driver?.error && !declined ? driver.error.message : null
  const current = installing ? DRIVER_STEPS.findIndex((s) => s.key === (driver?.progress ?? 'preparing')) : -1
  return (
    <section className="onb-step" aria-labelledby="onboarding-title">
      <span className="onb-icon" aria-hidden="true">
        <Icon name="shield" size={26} />
      </span>
      <h1 id="onboarding-title" className="onb-title display">
        One Windows component to add.
      </h1>
      <p className="onb-lead">Olsyn Drive uses WinFsp, a free and widely used Windows component, to show the drive as a letter in File Explorer. It comes with Olsyn Drive, so nothing is downloaded.</p>
      {!installing && (
        <div className="onb-expect">
          <Icon name="info" size={17} />
          <p>Windows will ask whether to allow changes to this PC. Choose Yes. This happens once.</p>
        </div>
      )}
      {installing && (
        <div role="status">
          <ol className="onb-progress" aria-label="Adding WinFsp">
            {DRIVER_STEPS.map((s, i) => (
              <li key={s.key} className={i < current ? 'is-done' : i === current ? 'is-current' : ''} aria-current={i === current ? 'step' : undefined}>
                <span className="onb-progress-mark" aria-hidden="true">
                  {i < current ? <Icon name="check" size={13} /> : i === current ? <Spinner size={13} /> : null}
                </span>
                {s.label}
                {s.key === 'approval' && i === current && <span className="onb-fine"> The prompt may be behind this window.</span>}
              </li>
            ))}
          </ol>
        </div>
      )}
      {declined && !installing && <InlineError message="Windows didn't add it because the prompt was declined. Try again and choose Yes, or ask your IT team to install WinFsp for you." />}
      {failed && !installing && <InlineError message={failed} />}
      {error && <InlineError message={error.message} />}
      <div className="onb-actions">
        <Button variant="primary" size="l" pending={installing} onClick={() => void run('install', () => client.installDriver())} data-autofocus>
          {declined || failed ? 'Try again' : 'Add WinFsp'}
        </Button>
      </div>
      {driver?.others?.includes('dokany') && <p className="onb-fine">Dokany is also on this PC. That's fine: the two work side by side.</p>}
    </section>
  )
}

function LetterStep({ onNext }: { onNext: () => void }) {
  const view = useAgentView()
  const { client } = useAgentContext()
  const [settings, setSettings] = useState<SettingsResponse | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [choice, setChoice] = useState<string | null>(null)
  const { pending, error, run } = useAction()
  const [connecting, setConnecting] = useState(false)
  const mount = view.state?.mount

  const load = () => {
    setLoadError(null)
    client
      .settings()
      .then((s) => {
        setSettings(s)
        const usable = s.choices.drive_letters
        setChoice((c) => (c && usable.includes(c) ? c : s.choices.suggested_letter || (usable.includes(s.settings.drive_letter) ? s.settings.drive_letter : (usable[0] ?? null))))
      })
      .catch((err: unknown) => setLoadError(errorText(err)))
  }
  useEffect(load, [client])

  useEffect(() => {
    if (connecting && mount?.state === 'mounted') onNext()
  }, [connecting, mount?.state, onNext])

  const letters = [...(settings?.choices.drive_letters ?? [])].sort()
  // The usual letter is taken on this PC (a disk, a network drive — even a
  // disconnected one — or subst): say so, next to the letter picked instead.
  const usual = settings?.settings.drive_letter ?? 'W:'
  const usualTakenBy = settings?.choices.letters_in_use?.[usual]
  const connect = () =>
    run('connect', async () => {
      try {
        setConnecting(true)
        await client.mount(choice ?? undefined)
      } catch (e) {
        setConnecting(false)
        load() // the letters may have changed meanwhile
        throw e
      }
    })

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    if (!choice || letters.length === 0) return
    const i = letters.indexOf(choice)
    let next = i
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') next = (i + 1) % letters.length
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') next = (i - 1 + letters.length) % letters.length
    else return
    e.preventDefault()
    setChoice(letters[next])
    ;(e.currentTarget.querySelector('[data-letter="' + letters[next] + '"]') as HTMLElement | null)?.focus()
  }

  const failed = mount?.state === 'failed' && connecting
  return (
    <section className="onb-step" aria-labelledby="onboarding-title">
      <h1 id="onboarding-title" className="onb-title display">
        Choose a letter for the drive.
      </h1>
      <p className="onb-lead">W: is the usual one, so paths match on every PC in the studio. Only letters free on this PC are shown.</p>
      {usualTakenBy && choice && choice !== usual && (
        <p className="onb-note" role="status">
          {usual} is already used on this PC by {letterUseText(usualTakenBy)}, so {choice} is selected.
        </p>
      )}
      {loadError && <InlineError message={loadError} onRetry={load} />}
      {!settings && !loadError && (
        <p className="code-wait">
          <Spinner size={14} /> Looking for free letters.
        </p>
      )}
      {settings && letters.length > 0 && (
        <div className="letters" role="radiogroup" aria-label="Drive letter" onKeyDown={onKeyDown}>
          {letters.map((l) => (
            <button
              key={l}
              type="button"
              role="radio"
              data-letter={l}
              aria-checked={choice === l}
              tabIndex={choice === l ? 0 : -1}
              className={'letter-tile' + (choice === l ? ' is-selected' : '')}
              onClick={() => setChoice(l)}
            >
              <span className="display">{l}</span>
            </button>
          ))}
        </div>
      )}
      {failed && <InlineError message={mount?.error?.message ?? "The drive couldn't be connected. Try another letter."} />}
      {error && <InlineError message={error.code === 'busy' ? 'Close files open on the drive, then try again.' : error.message} />}
      <div className="onb-actions">
        <Button variant="primary" size="l" disabled={!settings} pending={pending === 'connect' || (connecting && mount?.state === 'mounting')} onClick={() => void connect()}>
          {choice ? 'Connect ' + choice : 'Connect'}
        </Button>
      </div>
    </section>
  )
}

function letterUseText(kind: string): string {
  if (kind === 'network') return 'a network drive'
  if (kind === 'subst') return 'a folder shown as a drive'
  return 'another drive'
}

function DoneStep({ onDone }: { onDone: () => void }) {
  const view = useAgentView()
  const { client } = useAgentContext()
  const { pending, error, run } = useAction()
  const letter = view.state?.mount.letter ?? 'W:'
  return (
    <section className="onb-step onb-done" aria-labelledby="onboarding-title">
      <LetterMark letter={letter} tone="ok" progress={1} size={104} />
      <h1 id="onboarding-title" className="onb-title display">
        Your drive is {letter}
      </h1>
      <p className="onb-lead">Look for Olsyn Drive ({letter}) in File Explorer. Olsyn Drive keeps working from the tray, next to the clock.</p>
      <p className="onb-fine">{cacheExplainer}</p>
      {error && <InlineError message={error.message} />}
      <div className="onb-actions">
        <Button
          variant="primary"
          size="l"
          icon="folder"
          pending={pending === 'open'}
          data-autofocus
          onClick={() =>
            void run('open', async () => {
              await client.open({ target: 'drive', path: '/' })
              onDone()
            })
          }
        >
          {'Open ' + letter}
        </Button>
        <Button variant="ghost" onClick={onDone}>
          Close
        </Button>
      </div>
    </section>
  )
}
