import { useId, useState } from 'react'
import { AgentError } from '../../api/errors'
import { formatBytes, formatWhen, plural } from '../../lib/format'
import { useAction, useAgentContext, useAgentView } from '../../state/context'
import { Dialog } from '../../ui/Dialog'
import { Icon } from '../../ui/Icon'
import { Button, EmptyState } from '../../ui/controls'

export function Account({ onSignIn }: { onSignIn: () => void }) {
  const view = useAgentView()
  const { shell } = useAgentContext()
  const [signingOut, setSigningOut] = useState(false)
  const titleId = useId()
  const state = view.state
  const account = state?.account

  return (
    <section className="screen" aria-labelledby={titleId}>
      <header className="screen-head">
        <h1 id={titleId} className="screen-title display">
          Account
        </h1>
      </header>
      {!account ? (
        <EmptyState icon="user" title="You're signed out." action={<Button variant="primary" onClick={onSignIn}>Sign in</Button>}>
          Sign in to use the drive on this PC. Anything that had not reached the drive is still here and goes up when you sign in again.
        </EmptyState>
      ) : (
        <>
          <div className="account-card">
            <span className="avatar" aria-hidden="true">
              {initials(account.name)}
            </span>
            <div className="account-who">
              <p className="account-name display">{account.name}</p>
              <p className="account-email">{account.email}</p>
            </div>
          </div>
          <dl className="facts">
            <div>
              <dt>Drive</dt>
              <dd>
                <button type="button" className="link-btn" onClick={() => shell.openUrl(account.server)}>
                  {account.server.replace(/^https?:\/\//, '')}
                </button>
              </dd>
            </div>
            <div>
              <dt>This PC</dt>
              <dd>{state.device_name}</dd>
            </div>
            <div>
              <dt>Linked</dt>
              <dd>{formatWhen(account.linked_at)}</dd>
            </div>
            <div>
              <dt>Access</dt>
              <dd>{account.read_only ? 'View only. Files on the drive can be opened but not changed from this PC.' : 'View and change files'}</dd>
            </div>
          </dl>
          <p className="account-note">
            <Icon name="info" size={16} />
            You can see and remove this PC in the web app, under your devices. Removing it there signs it out here within a minute.
          </p>
          <div className="account-actions">
            <Button variant="danger-ghost" icon="key" onClick={() => setSigningOut(true)}>
              Sign out of this PC
            </Button>
          </div>
        </>
      )}
      {signingOut && <SignOutDialog onClose={() => setSigningOut(false)} />}
    </section>
  )
}

function initials(name: string) {
  return name
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((p) => p[0]!.toUpperCase())
    .join('')
}

/** Sign out, warning about files that have not reached the drive. */
export function SignOutDialog({ onClose }: { onClose: () => void }) {
  const view = useAgentView()
  const { client } = useAgentContext()
  const { pending, error, run } = useAction()
  const [clearCache, setClearCache] = useState(false)
  const [unsent, setUnsent] = useState<{ files: number; bytes: number; held?: number } | null>(null)
  const s = view.state?.transfers
  const waiting = (s?.files_left ?? 0) + (s?.held ?? 0)
  const letter = view.state?.mount.letter ?? 'The drive'

  const signOut = (confirmed: boolean) =>
    run('signout', async () => {
      try {
        await client.signOut({ clearCache, discardUnsentConfirmed: confirmed })
        onClose()
      } catch (err) {
        if (err instanceof AgentError && err.code === 'unsent_data') {
          setUnsent((err.detail.unsent as { files: number; bytes: number; held?: number }) ?? { files: waiting, bytes: 0 })
          return
        }
        throw err
      }
    })

  if (unsent) {
    return (
      <Dialog
        title={'Delete ' + plural(unsent.files, 'unsent file') + '?'}
        tone="danger"
        onClose={onClose}
        actions={
          <>
            <Button variant="ghost" onClick={() => setUnsent(null)} data-autofocus>
              Go back
            </Button>
            <Button variant="danger" pending={pending === 'signout'} onClick={() => void signOut(true)}>
              Delete and sign out
            </Button>
          </>
        }
      >
        <p>
          {plural(unsent.files, 'file')}
          {unsent.bytes > 0 ? ' (' + formatBytes(unsent.bytes) + ')' : ''} never reached the drive. Clearing this PC deletes them for good.
        </p>
        <p className="muted">To keep them, go back and sign out without clearing: they go up if you sign in here again.</p>
        {error && <p className="field-error" role="alert">{error.message}</p>}
      </Dialog>
    )
  }

  return (
    <Dialog
      title="Sign out of this PC?"
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose} data-autofocus>
            Stay signed in
          </Button>
          <Button variant="danger" pending={pending === 'signout'} onClick={() => void signOut(false)}>
            Sign out
          </Button>
        </>
      }
    >
      <p>{letter} disappears from File Explorer. Your files stay on the drive and in the web app.</p>
      {waiting > 0 && (
        <div className="warn-box" role="note">
          <Icon name="alert" size={17} />
          <p>
            {plural(waiting, 'file')} {waiting === 1 ? "hasn't" : "haven't"} reached the drive yet. {waiting === 1 ? 'It stays' : 'They stay'} on this PC and {waiting === 1 ? 'goes' : 'go'} up if you sign in here again.
          </p>
        </div>
      )}
      <label className="check-row">
        <input type="checkbox" checked={clearCache} onChange={(e) => setClearCache(e.target.checked)} />
        <span>Also clear files kept on this PC</span>
      </label>
      {error && <p className="field-error" role="alert">{error.message}</p>}
    </Dialog>
  )
}
