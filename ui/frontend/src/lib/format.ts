// Numbers and times in words a person reads at a glance. Sizes use 1024 steps
// with KB/MB/GB labels, the way Windows Explorer shows them, so the numbers here
// match the ones in Explorer's copy dialog.

const UNITS = ['KB', 'MB', 'GB', 'TB', 'PB']

/** 3 significant digits: "999 bytes", "1.00 KB", "12.4 MB", "3.21 GB", "512 GB". */
export function formatBytes(bytes: number): string {
  if (!Number.isFinite(bytes) || bytes < 0) return '0 bytes'
  if (bytes < 1000) return Math.round(bytes) + (Math.round(bytes) === 1 ? ' byte' : ' bytes')
  let value = bytes / 1024
  let unit = 0
  while (value >= 1000 && unit < UNITS.length - 1) {
    value /= 1024
    unit += 1
  }
  const digits = value >= 100 ? 0 : value >= 10 ? 1 : 2
  return value.toFixed(digits) + ' ' + UNITS[unit]
}

/** "11.2 MB/s"; an idle rate reads "0 KB/s". */
export function formatSpeed(bytesPerSecond: number): string {
  if (!Number.isFinite(bytesPerSecond) || bytesPerSecond <= 0) return '0 KB/s'
  if (bytesPerSecond < 1024) return '1 KB/s'
  return formatBytes(bytesPerSecond) + '/s'
}

/** "3 of 120 MB": both sides in the larger value's unit when that reads cleanly. */
export function formatProgress(done: number, total: number): string {
  return formatBytes(Math.min(done, total)) + ' of ' + formatBytes(total)
}

function roundSeconds(s: number): number {
  if (s < 20) return Math.max(5, Math.round(s / 5) * 5)
  return Math.round(s / 10) * 10
}

/**
 * Time left as a sentence: "A few seconds left", "About 40 seconds left",
 * "About 12 minutes left", "About 2 hours 10 minutes left", "More than a day left".
 * null or negative: "" (the caller shows nothing rather than a guess).
 */
export function formatTimeLeft(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds) || seconds < 0) return ''
  if (seconds < 5) return 'A few seconds left'
  if (seconds < 60) {
    const r = roundSeconds(seconds)
    return r >= 60 ? 'About 1 minute left' : 'About ' + r + ' seconds left'
  }
  if (seconds < 3600) {
    const m = Math.round(seconds / 60)
    if (m >= 60) return 'About 1 hour left'
    return 'About ' + m + (m === 1 ? ' minute' : ' minutes') + ' left'
  }
  if (seconds < 86400) {
    const h = Math.floor(seconds / 3600)
    const m = Math.round((seconds - h * 3600) / 60 / 5) * 5
    const hours = h + (h === 1 ? ' hour' : ' hours')
    if (m === 0) return 'About ' + hours + ' left'
    if (m === 60) return 'About ' + (h + 1) + ' hours left'
    return 'About ' + hours + ' ' + m + ' minutes left'
  }
  return 'More than a day left'
}

/** Compact time left for tight rows: "5 s", "40 s", "12 min", "2 h 10 min", "1 day+". */
export function formatTimeLeftShort(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined || !Number.isFinite(seconds) || seconds < 0) return ''
  if (seconds < 60) return Math.max(1, Math.round(seconds)) + ' s'
  if (seconds < 3600) return Math.round(seconds / 60) + ' min'
  if (seconds < 86400) {
    const h = Math.floor(seconds / 3600)
    const m = Math.round((seconds - h * 3600) / 60)
    return m === 0 || m === 60 ? h + (m === 60 ? 1 : 0) + ' h' : h + ' h ' + m + ' min'
  }
  return '1 day+'
}

export function plural(count: number, one: string, many: string = one + 's'): string {
  return count.toLocaleString('en') + ' ' + (count === 1 ? one : many)
}

const timeFormat = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' })
const dayFormat = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short' })
const yearFormat = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short', year: 'numeric' })

/** "Just now", "4 minutes ago", "Today at 14:05", "Yesterday at 09:12", "3 Nov at 10:00". */
export function formatWhen(iso: string | null | undefined, now: number = Date.now()): string {
  if (!iso) return ''
  const t = Date.parse(iso)
  if (Number.isNaN(t)) return ''
  const diff = Math.max(0, now - t) / 1000
  if (diff < 45) return 'Just now'
  if (diff < 3600) {
    const m = Math.max(1, Math.round(diff / 60))
    return m + (m === 1 ? ' minute ago' : ' minutes ago')
  }
  const date = new Date(t)
  const today = new Date(now)
  const startOfToday = new Date(today.getFullYear(), today.getMonth(), today.getDate()).getTime()
  if (t >= startOfToday) return 'Today at ' + timeFormat.format(date)
  if (t >= startOfToday - 86400_000) return 'Yesterday at ' + timeFormat.format(date)
  if (date.getFullYear() === today.getFullYear()) return dayFormat.format(date) + ' at ' + timeFormat.format(date)
  return yearFormat.format(date)
}

/** Seconds until an ISO time, never below 0. */
export function secondsUntil(iso: string | null | undefined, now: number = Date.now()): number {
  if (!iso) return 0
  const t = Date.parse(iso)
  return Number.isNaN(t) ? 0 : Math.max(0, Math.round((t - now) / 1000))
}

/** "9:58" countdown. */
export function formatCountdown(seconds: number): string {
  const s = Math.max(0, Math.floor(seconds))
  return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0')
}

/** The folder part of a drive path: "/Projects/Tower/Level 3.rvt" → "Projects / Tower". */
export function driveFolderOf(drivePath: string): string {
  const parts = drivePath.split('/').filter(Boolean)
  parts.pop()
  return parts.length ? parts.join(' / ') : 'Top of the drive'
}

/** A local path's last part. */
export function baseName(path: string): string {
  const parts = path.split(/[\\/]/).filter(Boolean)
  return parts[parts.length - 1] ?? path
}

/** Bandwidth limit in words: 0 = "No limit". */
export function formatLimit(bps: number): string {
  return bps <= 0 ? 'No limit' : formatSpeed(bps)
}
