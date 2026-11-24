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

/** What a finished upload's note means, in plain words. */
export function noteText(note: Transfer['note']): string | null {
  if (note === 'date_not_kept') return "Uploaded. The file's date could not be kept."
  return null
}

/** The conflict line: what happened, and where this PC's version went. */
export function conflictText(t: Transfer): string {
  const copy = t.conflict_copy ? t.conflict_copy.slice(t.conflict_copy.lastIndexOf('/') + 1) : null
  return 'Changed on the drive while you saved. Yours is kept beside it' + (copy ? ' as ' + copy : '') + '.'
}

/** The second line of a row, and whether it is a warning. Reasons are always written out, never only on hover. */
function subLine(t: Transfer): { text: string; warn: boolean } {
  if (t.state === 'waiting' && t.error) return { text: t.error.message, warn: true }
  if (t.state === 'failed' && t.error) return { text: t.error.message, warn: true }
  if (t.conflict_copy) return { text: conflictText(t), warn: true }
  const note = t.state === 'done' ? noteText(t.note) : null
  if (note) return { text: note, warn: true }
  if (t.origin === 'send') return { text: 'From ' + t.local_path, warn: false }
  return { text: driveFolderOf(t.drive_path), warn: false }
}

export function TransferRow({ t, now }: { t: Transfer & { receivedAt?: number }; now: number }) {
  const { client, shell } = useAgentContext()
  const { pending, error, run } = useAction()
  const done = interpolatedBytes(t, now)
  const verb = t.direction === 'up' ? 'Sending' : 'Fetching'
  const sub = subLine(t)

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
        {sub.warn ? <p className="trow-sub trow-why tone-warn">{sub.text}</p> : <Truncate text={sub.text} className="trow-sub" />}
        {error && <p className="trow-error">{error.message}</p>}
      </div>
      <div className="trow-right">{right}</div>
      <div className="trow-actions">
        {t.conflict_copy && (
          <Button
            size="s"
            variant="secondary"
            icon="copy"
            pending={pending === 'conflict'}
            onClick={() => void run('conflict', () => client.open({ target: 'web', path: t.conflict_copy! }).catch(() => shell.openUrl('https://drive.olsyn.com')))}
          >
            Show both
          </Button>
        )}
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
