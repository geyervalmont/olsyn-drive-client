// The tray popover: what is happening in one line, what is moving, what just
// finished, and the four things people reach for.

import type { State, Transfer } from '../../api/types'
import { formatBytes, formatProgress, formatSpeed, formatTimeLeftShort, formatWhen } from '../../lib/format'
import { interpolatedBytes, useNow } from '../../lib/useNow'
import { statusLine } from '../../state/derive'
import { useAction, useAgentContext, useAgentView } from '../../state/context'
import { FileName } from '../../ui/FileName'
import { Icon } from '../../ui/Icon'
import { LetterMark } from '../../ui/LetterMark'
import { ProgressBar } from '../../ui/Progress'
import { Button, IconButton } from '../../ui/controls'
import { Banners } from '../Banners'
import { PauseControl } from '../main/Transfers'

export function Popover() {
  const view = useAgentView()
  const { client, shell } = useAgentContext()
  const { error, run } = useAction()
  const state = view.state
  const line = statusLine(view)
  const moving = view.live.filter((t) => t.state === 'active').slice(0, 4)
  const now = useNow(moving.length ? 200 : 30_000)
  const letter = state?.mount.letter ?? 'W:'
  const open = (route: string) => {
    shell.showMain(route)
    shell.hidePopover()
  }
  const s = state?.transfers

  return (
    <main className={'popover tone-' + line.tone + (view.stream !== 'live' && state ? ' is-stale' : '')} aria-label="Olsyn Drive">
      <header className="pop-head">
        <LetterMark letter={letter} tone={line.tone} progress={line.progress} size={46} />
        <div className="pop-status" aria-live="polite">
          <p className="pop-title display">{line.title}</p>
          <p className="pop-detail">{line.detail}</p>
        </div>
        <IconButton icon="external" label="Open the Olsyn Drive window" onClick={() => open('transfers/active')} />
      </header>

      {state && s && s.files_left > 0 && (
        <div className="pop-progress">
          <ProgressBar value={s.bytes_total ? s.bytes_done / s.bytes_total : null} label="Overall progress" tone={state.paused ? 'paused' : 'busy'} />
          <div className="pop-progress-row">
            <span className="tnum">{state.paused ? 'Paused' : s.up_bps > 0 ? formatSpeed(s.up_bps) : 'Waiting'}</span>
            <PauseControl compact />
          </div>
        </div>
      )}

      <Banners navigate={open} compact />

      {/* Signed out by the drive: the banner above has its own Sign in. */}
      {!state?.account && state && state.reason?.code !== 'token_revoked' && (
        <div className="pop-signin">
          <Button variant="primary" onClick={() => open('signin')}>
            {state.signin?.state === 'waiting' ? 'Show the code' : 'Sign in'}
          </Button>
        </div>
      )}

      {moving.length > 0 && (
        <section className="pop-section" aria-label="Sending now">
          <h2 className="pop-section-title">Now</h2>
          <ul className="pop-list">
            {moving.map((t) => (
              <PopRow key={t.id} t={t} now={now} />
            ))}
          </ul>
        </section>
      )}

      {view.held.length > 0 && (
        <button type="button" className="pop-held" onClick={() => open('transfers/held')}>
          <Icon name="hold" size={17} />
          <span>
            {view.held.length === 1 ? '1 file needs you' : view.held.length + ' files need you'}
          </span>
          <Icon name="chevronRight" size={15} />
        </button>
      )}

      {state?.account && (
        <section className="pop-section" aria-label="Recently finished">
          <h2 className="pop-section-title">Recent</h2>
          {view.recent.length === 0 ? (
            <p className="pop-empty">Files you save on {letter} show up here once they are uploaded.</p>
          ) : (
            <ul className="pop-list">
              {view.recent.slice(0, 5).map((t) => (
                <li key={t.id} className="pop-row is-done">
                  <span className={'trow-icon state-' + t.state} aria-hidden="true">
                    <Icon name={t.state === 'done' ? 'check' : t.state === 'skipped' ? 'skip' : 'alert'} size={14} />
                  </span>
                  <FileName name={t.name} className="pop-name" />
                  <span className="pop-meta">{t.state === 'skipped' ? 'Already there' : formatWhen(t.finished_at, now)}</span>
                </li>
              ))}
            </ul>
          )}
        </section>
      )}

      {state?.account && <CacheLine cache={state.cache} onOpen={() => open('settings')} />}

      {error && <p className="field-error pop-error" role="alert">{error.message}</p>}

      <footer className="pop-actions">
        <button
          type="button"
          className="pop-action"
          disabled={state?.mount.state !== 'mounted'}
          onClick={() => void run('drive', () => client.open({ target: 'drive', path: '/' }))}
        >
          <Icon name="folder" size={19} />
          <span>{'Open ' + letter}</span>
        </button>
        <button type="button" className="pop-action" disabled={!state?.account} onClick={() => void run('web', () => client.open({ target: 'web' }))}>
          <Icon name="globe" size={19} />
          <span>Open in browser</span>
        </button>
        <button type="button" className="pop-action" onClick={() => open('activity')}>
          <Icon name="activity" size={19} />
          <span>Activity</span>
        </button>
        <button type="button" className="pop-action" onClick={() => open('settings')}>
          <Icon name="gear" size={19} />
          <span>Settings</span>
        </button>
      </footer>
    </main>
  )
}

const GiB = 1024 * 1024 * 1024

/** One quiet line once the cache holds more than 1 GiB: how much, and why. */
export function CacheLine({ cache, onOpen }: { cache: State['cache']; onOpen: () => void }) {
  if (!cache || cache.used_bytes <= GiB) return null
  const why =
    cache.unsent_bytes > 0 ? formatBytes(cache.unsent_bytes) + ' still uploading' : cache.uploaded_bytes > 0 ? 'uploaded files clear soon' : 'recently opened files'
  return (
    <button type="button" className="pop-cache" onClick={onOpen}>
      {formatBytes(cache.used_bytes) + ' kept on this PC: ' + why + '.'}
    </button>
  )
}

function PopRow({ t, now }: { t: Transfer & { receivedAt?: number }; now: number }) {
  const done = interpolatedBytes(t, now)
  return (
    <li className="pop-row">
      <span className="trow-icon state-active" aria-hidden="true">
        <Icon name={t.direction === 'up' ? 'up' : 'down'} size={14} />
      </span>
      <div className="pop-row-main">
        <FileName name={t.name} className="pop-name" />
        <ProgressBar value={t.size ? done / t.size : null} label={'Sending ' + t.name} size="s" valueText={formatProgress(done, t.size)} />
      </div>
      <span className="pop-meta tnum">{t.eta_seconds !== null && t.eta_seconds > 2 ? formatTimeLeftShort(t.eta_seconds) : formatBytes(t.size)}</span>
    </li>
  )
}
