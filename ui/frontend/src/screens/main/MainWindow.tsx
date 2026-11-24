// The main window: a navy frame on the left (as in the drive's web app) with the
// drive letter and its state, and the sections on paper to the right.
// Dropping a folder anywhere on the window sends it to the drive.

import { useEffect, useState, type DragEvent } from 'react'
import { statusLine } from '../../state/derive'
import { useAgentContext, useAgentView } from '../../state/context'
import { Icon, type IconName } from '../../ui/Icon'
import { LetterMark } from '../../ui/LetterMark'
import { Banners } from '../Banners'
import { About } from './About'
import { Account } from './Account'
import { Activity } from './Activity'
import { QuitDialog } from './QuitDialog'
import { SendDialog } from './SendDialog'
import { Settings } from './Settings'
import { Transfers, transferTabs, type TransferTab } from './Transfers'

type Section = 'transfers' | 'activity' | 'settings' | 'account' | 'about'

const nav: { key: Section; label: string; icon: IconName }[] = [
  { key: 'transfers', label: 'Transfers', icon: 'transfers' },
  { key: 'activity', label: 'Activity', icon: 'activity' },
  { key: 'settings', label: 'Settings', icon: 'gear' },
  { key: 'account', label: 'Account', icon: 'user' },
  { key: 'about', label: 'About', icon: 'info' },
]

export function parseRoute(route: string): { section: Section; tab: TransferTab } {
  const [head, sub] = route.replace(/^#?\/?/, '').split('/')
  const section = (nav.some((n) => n.key === head) ? head : 'transfers') as Section
  const tab = (transferTabs as string[]).includes(sub ?? '') ? (sub as TransferTab) : 'active'
  return { section, tab }
}

export function MainWindow({ route, navigate }: { route: string; navigate: (route: string) => void }) {
  const view = useAgentView()
  const { shell } = useAgentContext()
  const { section, tab } = parseRoute(route)
  const [sendPaths, setSendPaths] = useState<string[] | null>(null)
  const [dragging, setDragging] = useState(false)
  const quitting = route === 'quit'
  const line = statusLine(view)
  const state = view.state
  const canSend = !!state?.account && !state.account.read_only

  useEffect(
    () =>
      shell.onDrop((paths) => {
        setDragging(false)
        if (paths.length && canSend) setSendPaths(paths)
      }),
    [shell, canSend],
  )

  // Keyboard: Ctrl+1..5 jump between sections.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (!(e.ctrlKey || e.metaKey) || e.altKey) return
      const n = Number(e.key)
      if (n >= 1 && n <= nav.length) {
        e.preventDefault()
        navigate(nav[n - 1].key)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [navigate])

  const onDragEnter = (e: DragEvent) => {
    if (e.dataTransfer?.types?.includes('Files') && canSend) setDragging(true)
  }
  const onDragLeave = (e: DragEvent) => {
    if (e.currentTarget === e.target || !e.currentTarget.contains(e.relatedTarget as Node)) setDragging(false)
  }

  return (
    <div className={'app' + (view.stream !== 'live' && state ? ' is-stale' : '')} data-file-drop-target onDragEnter={onDragEnter} onDragLeave={onDragLeave} onDrop={() => setDragging(false)}>
      <a className="skip-link" href="#content">
        Skip to content
      </a>
      <nav className="frame" aria-label="Olsyn Drive">
        <div className="frame-head">
          <LetterMark letter={state?.mount.letter ?? 'W:'} tone={line.tone} progress={line.progress} size={42} label={'Drive ' + (state?.mount.letter ?? 'W:') + ': ' + line.title} />
          <div className="frame-brand">
            <p className="frame-name display">Olsyn Drive</p>
            <p className="frame-state">{line.title}</p>
          </div>
        </div>
        <ul className="frame-nav">
          {nav.map((n, i) => {
            const count = n.key === 'transfers' ? view.held.length : 0
            return (
              <li key={n.key}>
                <button
                  type="button"
                  className={'frame-link' + (section === n.key && !quitting ? ' is-active' : '')}
                  aria-current={section === n.key && !quitting ? 'page' : undefined}
                  onClick={() => navigate(n.key === 'transfers' ? 'transfers/' + tab : n.key)}
                  title={n.label + ' (Ctrl+' + (i + 1) + ')'}
                >
                  <Icon name={n.icon} size={17} />
                  <span>{n.label}</span>
                  {count > 0 && (
                    <span className="frame-badge" aria-label={count + ' held'}>
                      {count}
                    </span>
                  )}
                </button>
              </li>
            )
          })}
        </ul>
        <div className="frame-foot">
          {state?.account && (
            <p className="frame-who" title={state.account.email}>
              <Icon name="user" size={14} />
              <span className="truncate">{state.account.name}</span>
            </p>
          )}
        </div>
      </nav>
      <div className="content" id="content" tabIndex={-1}>
        <Banners navigate={navigate} />
        {section === 'transfers' && <Transfers tab={tab} onTab={(t) => navigate('transfers/' + t)} onSend={() => setSendPaths([])} />}
        {section === 'activity' && <Activity navigate={navigate} />}
        {section === 'settings' && <Settings />}
        {section === 'account' && <Account onSignIn={() => navigate('signin')} />}
        {section === 'about' && <About />}
      </div>
      {dragging && (
        <div className="drop-overlay" aria-hidden="true">
          <div className="drop-card">
            <Icon name="send" size={28} />
            <p className="display">Drop to send to the drive</p>
            <p>You choose the folder next.</p>
          </div>
        </div>
      )}
      {sendPaths && (
        <SendDialog
          initialPaths={sendPaths}
          onClose={() => setSendPaths(null)}
          onSent={() => {
            setSendPaths(null)
            navigate('transfers/waiting')
          }}
        />
      )}
      {quitting && <QuitDialog onClose={() => navigate(section)} />}
    </div>
  )
}
