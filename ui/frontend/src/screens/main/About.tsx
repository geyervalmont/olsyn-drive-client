import { useId, useState } from 'react'
import { formatBytes, formatWhen } from '../../lib/format'
import { frontEndLicences, goLicences, winfspNotice, winfspRepo, type Licence } from '../../licences'
import { useAction, useAgentContext, useAgentView } from '../../state/context'
import { Icon } from '../../ui/Icon'
import { ProgressBar } from '../../ui/Progress'
import { useToast } from '../../ui/Toasts'
import { Button, InlineError } from '../../ui/controls'

export function About() {
  const view = useAgentView()
  const { client } = useAgentContext()
  const toast = useToast()
  const titleId = useId()
  const diag = useAction()
  const [bundle, setBundle] = useState<{ path: string; size: number } | null>(null)
  const state = view.state

  return (
    <section className="screen" aria-labelledby={titleId}>
      <header className="screen-head">
        <h1 id={titleId} className="screen-title display">
          About
        </h1>
      </header>

      <div className="about-grid">
        <div className="about-card">
          <p className="about-product display">Olsyn Drive</p>
          <dl className="facts">
            <div>
              <dt>Version</dt>
              <dd className="tnum">{state?.version ?? 'unknown'}</dd>
            </div>
            <div>
              <dt>Windows component</dt>
              <dd>{state ? driverText(state.driver.state, state.driver.version) : 'unknown'}</dd>
            </div>
            <div>
              <dt>Running since</dt>
              <dd>{state ? formatWhen(state.started_at) : ''}</dd>
            </div>
          </dl>
        </div>
        <UpdateCard />
      </div>

      <section className="about-section" aria-labelledby={titleId + '-help'}>
        <h2 id={titleId + '-help'} className="settings-title">
          Getting help
        </h2>
        <p className="about-text">If something isn't working, create a diagnostics file and send it to whoever looks after your IT. It holds no file names, no passwords and no content.</p>
        <div className="about-actions">
          <Button
            variant="secondary"
            icon="lifebuoy"
            pending={diag.pending === 'diag'}
            onClick={() =>
              void diag.run('diag', async () => {
                const r = await client.diagnostics()
                setBundle(r)
                toast({ icon: 'check', title: 'Diagnostics file created', body: r.path })
              })
            }
          >
            Create diagnostics file
          </Button>
          <Button variant="ghost" icon="folder" onClick={() => void client.open({ target: 'logs' }).catch(() => undefined)}>
            Open logs folder
          </Button>
        </div>
        {bundle && (
          <p className="about-result">
            <Icon name="check" size={15} /> Saved {formatBytes(bundle.size)} to <span className="path-inline" title={bundle.path}>{bundle.path}</span>{' '}
            <button type="button" className="link-btn" onClick={() => void client.open({ target: 'folder', path: parentOf(bundle.path) }).catch(() => undefined)}>
              Show in folder
            </button>
          </p>
        )}
        {diag.error && <InlineError message={diag.error.message} />}
      </section>

      <section className="about-section" aria-labelledby={titleId + '-lic'}>
        <h2 id={titleId + '-lic'} className="settings-title">
          Open-source licences
        </h2>
        <p className="about-text">Olsyn Drive is open source. It is built with these components, each under its own licence.</p>
        <p className="about-text">
          {winfspNotice}.{' '}
          <a href={winfspRepo} target="_blank" rel="noreferrer">
            {winfspRepo}
          </a>
        </p>
        <LicenceTable items={[...goLicences, ...frontEndLicences]} />
        <FullNotices />
      </section>
    </section>
  )
}

/** The full licence texts, read from the agent when opened. */
function FullNotices() {
  const { client } = useAgentContext()
  const [text, setText] = useState<string | null>(null)
  const [failed, setFailed] = useState(false)
  return (
    <details
      className="notice"
      onToggle={(e) => {
        if (!(e.currentTarget as HTMLDetailsElement).open || text !== null) return
        client
          .licences()
          .then((t) => (t === null ? setFailed(true) : setText(t)))
          .catch(() => setFailed(true))
      }}
    >
      <summary>Full licence texts</summary>
      {failed ? <p className="about-text">The licence texts ship with the program as NOTICE.txt.</p> : <pre>{text ?? 'Loading…'}</pre>}
    </details>
  )
}

function parentOf(path: string) {
  const i = Math.max(path.lastIndexOf('\\'), path.lastIndexOf('/'))
  return i > 0 ? path.slice(0, i) : path
}

function driverText(state: string, version: string | null) {
  switch (state) {
    case 'ok':
      return 'WinFsp ' + (version ?? '')
    case 'missing':
      return 'Not installed'
    case 'outdated':
      return 'WinFsp ' + (version ?? '') + ', needs an update'
    case 'installing':
      return 'Installing'
    case 'restart_needed':
      return 'WinFsp ' + (version ?? '') + ', waiting for Windows to sign out and in'
    case 'not_needed':
      return 'Not needed on this system'
    default:
      return 'Unknown'
  }
}

function LicenceTable({ items }: { items: Licence[] }) {
  return (
    <table className="licences">
      <thead>
        <tr>
          <th scope="col">Component</th>
          <th scope="col">Version</th>
          <th scope="col">Licence</th>
        </tr>
      </thead>
      <tbody>
        {items.map((l) => (
          <tr key={l.name}>
            <td>{l.name}</td>
            <td className="tnum">{l.version}</td>
            <td>{l.licence}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

export function UpdateCard() {
  const view = useAgentView()
  const { client } = useAgentContext()
  const { pending, error, run } = useAction()
  const u = view.state?.update
  if (!u) return null
  const progress = u.progress && u.progress.bytes_total > 0 ? u.progress.bytes_done / u.progress.bytes_total : null
  let title = 'Up to date.'
  let body = u.checked_at ? 'Checked ' + formatWhen(u.checked_at).replace(/^[A-Z]/, (c) => c.toLowerCase()) + '.' : ''
  let action: { label: string; run: () => Promise<unknown>; primary?: boolean } | null = { label: 'Check for updates', run: () => client.checkUpdate() }
  switch (u.state) {
    case 'idle':
      title = 'Not checked yet.'
      body = ''
      break
    case 'checking':
      title = 'Checking for updates.'
      action = null
      break
    case 'available':
      title = 'Version ' + u.available + ' is available.'
      body = u.notes ?? ''
      action = { label: 'Install update', run: () => client.applyUpdate('idle'), primary: true }
      break
    case 'downloading':
      title = 'Downloading version ' + u.available + '.'
      body = u.progress ? formatBytes(u.progress.bytes_done) + ' of ' + formatBytes(u.progress.bytes_total) : ''
      action = null
      break
    case 'ready':
      title = 'Version ' + u.available + ' is ready.'
      body = 'It installs when nothing is moving. The drive letter comes back by itself.'
      action = { label: 'Restart now', run: () => client.applyUpdate('now'), primary: true }
      break
    case 'applying':
      title = 'Installing version ' + u.available + '.'
      body = 'Olsyn Drive restarts in a moment.'
      action = null
      break
    case 'failed':
      title = "The update didn't install."
      body = (u.error?.message ?? '') + ' Still on version ' + u.current + '.'
      action = { label: 'Try again', run: () => client.checkUpdate() }
      break
  }
  return (
    <div className={'about-card update-card state-' + u.state} aria-live="polite">
      <p className="update-title">
        <Icon name={u.state === 'failed' ? 'alert' : u.state === 'up_to_date' ? 'check' : 'sparkle'} size={17} />
        {title}
      </p>
      {body && <p className="update-body">{body}</p>}
      {(u.state === 'downloading' || u.state === 'applying') && <ProgressBar value={u.state === 'applying' ? null : progress} label="Update download" />}
      {u.driver_required && <p className="update-body">This version also needs a newer Windows component. You'll be asked once.</p>}
      {error && <InlineError message={error.message} />}
      {action && (
        <Button size="s" variant={action.primary ? 'primary' : 'secondary'} pending={pending === 'u'} onClick={() => void run('u', action!.run)}>
          {action.label}
        </Button>
      )}
    </div>
  )
}
