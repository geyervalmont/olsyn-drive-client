import type { ReactNode } from 'react'
import type { Transfer } from '../../api/types'
import { driveFolderOf, formatBytes, formatProgress, formatSpeed, formatTimeLeftShort, formatWhen } from '../../lib/format'
import { interpolatedBytes } from '../../lib/useNow'
import { useAction, useAgentContext } from '../../state/context'
import { Icon, type IconName } from '../../ui/Icon'
import { FileName, Truncate } from '../../ui/FileName'
import { ProgressBar } from '../../ui/Progress'
import { Button } from '../../ui/controls'

export const ROW_HEIGHT = 60

function rowIcon(t: Transfer): IconName {
  if (t.state === 'done') return 'check'
  if (t.state === 'skipped') return 'skip'
  if (t.state === 'failed' || t.state === 'held') return 'alert'
  if (t.state === 'cancelled') return 'close'
  if (t.state === 'waiting') return t.error ? 'clock' : t.direction === 'up' ? 'up' : 'down'
  return t.direction === 'up' ? 'up' : 'down'
}

function subLine(t: Transfer): string {
  if (t.state === 'waiting' && t.error) return t.error.message
  if (t.state === 'failed' && t.error) return t.error.message
  if (t.origin === 'send') return 'From ' + t.local_path
  return driveFolderOf(t.drive_path)
}

export function TransferRow({ t, now }: { t: Transfer & { receivedAt?: number }; now: number }) {
  const { client, shell } = useAgentContext()
  const { pending, error, run } = useAction()
  const done = interpolatedBytes(t, now)
  const verb = t.direction === 'up' ? 'Sending' : 'Fetching'

  let right: ReactNode = null
  if (t.state === 'active') {
    right = (
      <div className="trow-progress">
        <ProgressBar value={t.size ? done / t.size : null} label={verb + ' ' + t.name} size="s" valueText={formatProgress(done, t.size)} />
        <p className="trow-meta tnum">
          <span>{formatProgress(done, t.size)}</span>
          <span>{formatSpeed(t.bps)}</span>
          {t.eta_seconds !== null && <span>{formatTimeLeftShort(t.eta_seconds)} left</span>}
        </p>
      </div>
    )
  } else if (t.state === 'waiting') {
    right = (
      <p className="trow-meta">
        {t.bytes_done > 0 ? formatProgress(t.bytes_done, t.size) + ' sent, waiting' : formatBytes(t.size) + ', waiting'}
      </p>
    )
  } else if (t.state === 'done') {
    right = <p className="trow-meta">{(t.direction === 'up' ? 'Sent ' : 'Fetched ') + formatWhen(t.finished_at, now).replace(/^[A-Z]/, (c) => c.toLowerCase())}</p>
  } else if (t.state === 'skipped') {
    right = <p className="trow-meta">{t.skipped_reason === 'junk' ? 'Windows system file, not uploaded' : 'Already on the drive'}</p>
  } else if (t.state === 'cancelled') {
    right = <p className="trow-meta">Cancelled</p>
  } else if (t.state === 'failed') {
    right = <p className="trow-meta tone-danger">Not uploaded</p>
  }

  return (
    <div className={'trow state-' + t.state}>
      <span className={'trow-icon state-' + t.state} aria-hidden="true">
        <Icon name={rowIcon(t)} size={16} />
      </span>
      <div className="trow-main">
        <FileName name={t.name} className="trow-name" />
        <Truncate text={subLine(t)} className={'trow-sub' + (t.error && (t.state === 'waiting' || t.state === 'failed') ? ' tone-warn' : '')} />
        {t.conflict_copy && (
          <p className="trow-conflict">
            <Icon name="copy" size={14} /> Changed on the drive while you saved. Your version was kept as a second copy.{' '}
            <button type="button" className="link-btn" onClick={() => void client.open({ target: 'web', path: t.conflict_copy! }).catch(() => shell.openUrl('https://drive.olsyn.com'))}>
              Show
            </button>
          </p>
        )}
        {error && <p className="trow-error">{error.message}</p>}
      </div>
      <div className="trow-right">{right}</div>
      <div className="trow-actions">
        {t.can.retry && (
          <Button size="s" variant="secondary" icon="refresh" pending={pending === 'retry'} onClick={() => void run('retry', () => client.retry(t.id))}>
            Try again
          </Button>
        )}
        {t.can.cancel && (t.state === 'active' || t.state === 'waiting') && (
          <Button size="s" variant="ghost" pending={pending === 'cancel'} onClick={() => void run('cancel', () => client.cancel(t.id))}>
            Cancel
          </Button>
        )}
      </div>
    </div>
  )
}
