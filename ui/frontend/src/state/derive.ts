// Plain-words summaries of the agent's state, shared by the popover, the main
// window and the shell's tray tooltip.

import { formatBytes, formatTimeLeft, plural } from '../lib/format'
import type { State } from '../api/types'
import type { AgentView } from './reducer'

export type Tone = 'ok' | 'busy' | 'paused' | 'warn' | 'off' | 'danger'

export interface StatusLine {
  tone: Tone
  /** One short sentence, set as the title. */
  title: string
  /** A second sentence with what is happening or what to do. */
  detail: string
  /** 0..1 when something measurable is moving, else null. */
  progress: number | null
}

const timeOnly = (iso: string) =>
  new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(new Date(iso))

export function workingLine(state: State): StatusLine {
  const s = state.transfers
  const left = Math.max(0, s.bytes_total - s.bytes_done)
  const progress = s.bytes_total > 0 ? Math.min(1, s.bytes_done / s.bytes_total) : null
  const sending = s.up_bps > 0 || s.down_bps === 0
  const verb = sending ? 'Sending' : 'Fetching'
  const eta = formatTimeLeft(s.eta_seconds)
  const detail = [formatBytes(left) + ' to go', eta ? eta.charAt(0).toLowerCase() + eta.slice(1) : '']
    .filter(Boolean)
    .join(', ')
  return {
    tone: 'busy',
    title: verb + ' ' + plural(s.files_left, 'file') + '.',
    detail: s.files_left > 0 ? detail + '.' : 'Finishing up.',
    progress,
  }
}

export function statusLine(view: AgentView): StatusLine {
  const state = view.state
  if (!state) {
    if (view.stream === 'unreachable') {
      return { tone: 'danger', title: "Olsyn Drive isn't running.", detail: 'Starting it again. This usually takes a few seconds.', progress: null }
    }
    return { tone: 'off', title: 'Starting Olsyn Drive.', detail: 'One moment.', progress: null }
  }
  const letter = state.mount.letter ?? 'The drive'
  switch (state.connection) {
    case 'signed_out':
      if (state.reason?.code === 'token_revoked') {
        return { tone: 'warn', title: 'Signed out by the drive.', detail: 'This PC was removed from your account. Sign in again to carry on.', progress: null }
      }
      return { tone: 'off', title: "You're signed out.", detail: state.reason?.message || 'Sign in to use the drive on this PC.', progress: null }
    case 'linking':
      return { tone: 'busy', title: 'Waiting for your browser.', detail: 'Approve this PC in the browser window that opened.', progress: null }
    case 'starting':
      return { tone: 'busy', title: 'Getting your drive ready.', detail: 'This takes a few seconds.', progress: null }
    case 'offline':
      return {
        tone: 'off',
        title: "Can't reach the drive.",
        detail: state.transfers.files_left > 0
          ? plural(state.transfers.files_left, 'file') + ' will go up when you are back online.'
          : 'Files you save stay on this PC until you are back online.',
        progress: null,
      }
    case 'server_restarting':
      return { tone: 'paused', title: 'The drive is restarting.', detail: 'Uploads carry on by themselves in a moment.', progress: null }
    case 'attention': {
      const issue = state.issues[0]
      return { tone: 'warn', title: 'Something needs you.', detail: issue?.message ?? 'Open Olsyn Drive to see what.', progress: null }
    }
    case 'paused':
      return {
        tone: 'paused',
        title: 'Paused.',
        detail: state.paused_until
          ? 'Uploads start again at ' + timeOnly(state.paused_until) + '.'
          : state.transfers.files_left > 0
            ? plural(state.transfers.files_left, 'file') + ' waiting until you resume.'
            : 'Nothing is waiting.',
        progress: state.transfers.bytes_total > 0 ? state.transfers.bytes_done / state.transfers.bytes_total : null,
      }
    case 'working':
      return workingLine(state)
    case 'connected':
      return {
        tone: 'ok',
        title: 'Everything is on the drive.',
        detail: state.mount.state === 'mounted' ? letter + ' is ready.' : 'The drive is not connected to a letter.',
        progress: null,
      }
    default:
      return { tone: 'warn', title: 'Something needs you.', detail: 'Open Olsyn Drive to see what.', progress: null }
  }
}
