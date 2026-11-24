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

/** The one-line answer to "is the drive filling up this PC?": first run and Settings. */
export const cacheExplainer = 'Files live on the Olsyn drive. This PC keeps only what you opened recently and what is still uploading.'

/** "Uploading 3 of 120 · 1.2 GB left" (agents before 1.2 have no files_done: "Uploading 120 files · …"). */
export function uploadingTitle(s: State['transfers']): string {
  const left = formatBytes(Math.max(0, s.bytes_total - s.bytes_done)) + ' left'
  if (s.files_done === undefined) return 'Uploading ' + plural(s.files_left, 'file') + ' · ' + left
  const total = s.files_done + s.files_left
  return 'Uploading ' + Math.min(s.files_done + 1, total).toLocaleString('en-US') + ' of ' + total.toLocaleString('en-US') + ' · ' + left
}

export function workingLine(state: State): StatusLine {
  const s = state.transfers
  const progress = s.bytes_total > 0 ? Math.min(1, s.bytes_done / s.bytes_total) : null
  if (s.files_left === 0) {
    // only reads: files opened on this PC coming down from the drive
    return { tone: 'busy', title: 'Opening files from the drive', detail: s.down_bps > 0 ? 'Fetching what you opened.' : 'Finishing up.', progress }
  }
  const eta = formatTimeLeft(s.eta_seconds)
  return {
    tone: 'busy',
    title: uploadingTitle(s),
    detail: eta ? eta + '.' : 'Files stay on this PC until they are uploaded.',
    progress,
  }
}

export function statusLine(view: AgentView): StatusLine {
  const state = view.state
  if (!state) {
    if (view.blocked) {
      return { tone: 'danger', title: view.blocked.title, detail: view.blocked.detail, progress: null }
    }
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
          ? plural(state.transfers.files_left, 'file') + ' will upload when you are back online.'
          : 'Files you save wait on this PC and upload when you are back online.',
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
        title: 'All changes are uploaded',
        detail: state.mount.state === 'mounted' ? letter + ' is ready.' : 'The drive is not connected to a letter.',
        progress: null,
      }
    default:
      return { tone: 'warn', title: 'Something needs you.', detail: 'Open Olsyn Drive to see what.', progress: null }
  }
}
