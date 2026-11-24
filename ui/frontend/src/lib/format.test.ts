import { describe, expect, it } from 'vitest'
import {
  baseName,
  driveFolderOf,
  formatBytes,
  formatCountdown,
  formatLimit,
  formatProgress,
  formatSpeed,
  formatTimeLeft,
  formatTimeLeftShort,
  formatWhen,
  plural,
  secondsUntil,
} from './format'

const KB = 1024
const MB = KB * 1024
const GB = MB * 1024

describe('formatBytes', () => {
  it.each([
    [0, '0 bytes'],
    [1, '1 byte'],
    [999, '999 bytes'],
    [1000, '0.98 KB'],
    [1024, '1.00 KB'],
    [12.4 * MB, '12.4 MB'],
    [512 * MB, '512 MB'],
    [3 * GB, '3.00 GB'],
    [3.21 * GB, '3.21 GB'],
    [1000 * GB, '0.98 TB'],
    [-5, '0 bytes'],
    [Number.NaN, '0 bytes'],
  ])('%s → %s', (n, s) => expect(formatBytes(n)).toBe(s))
})

describe('formatSpeed', () => {
  it('reads like Explorer', () => {
    expect(formatSpeed(11.2 * MB)).toBe('11.2 MB/s')
    expect(formatSpeed(300 * KB)).toBe('300 KB/s')
  })
  it('never shows a tiny or negative rate as bytes', () => {
    expect(formatSpeed(0)).toBe('0 KB/s')
    expect(formatSpeed(-1)).toBe('0 KB/s')
    expect(formatSpeed(200)).toBe('1 KB/s')
  })
  it('limit 0 is no limit', () => {
    expect(formatLimit(0)).toBe('No limit')
    expect(formatLimit(5 * MB)).toBe('5.00 MB/s')
  })
})

describe('formatTimeLeft', () => {
  it.each([
    [null, ''],
    [-1, ''],
    [3, 'A few seconds left'],
    [12, 'About 10 seconds left'],
    [42, 'About 40 seconds left'],
    [58, 'About 1 minute left'],
    [61, 'About 1 minute left'],
    [12 * 60 + 10, 'About 12 minutes left'],
    [3599, 'About 1 hour left'],
    [3600 * 2 + 600, 'About 2 hours 10 minutes left'],
    [3600 * 3, 'About 3 hours left'],
    [3600 * 5 + 59 * 60, 'About 6 hours left'],
    [90000, 'More than a day left'],
  ])('%s → %s', (s, text) => expect(formatTimeLeft(s as number | null)).toBe(text))

  it('has a compact form', () => {
    expect(formatTimeLeftShort(40)).toBe('40 s')
    expect(formatTimeLeftShort(0.2)).toBe('1 s')
    expect(formatTimeLeftShort(12 * 60)).toBe('12 min')
    expect(formatTimeLeftShort(3600 * 2 + 600)).toBe('2 h 10 min')
    expect(formatTimeLeftShort(3600)).toBe('1 h')
    expect(formatTimeLeftShort(100000)).toBe('1 day+')
    expect(formatTimeLeftShort(null)).toBe('')
  })
})

describe('other formatters', () => {
  it('progress, plural, countdown', () => {
    expect(formatProgress(104857600, 524288000)).toBe('100 MB of 500 MB')
    expect(formatProgress(600, 500)).toBe('500 bytes of 500 bytes')
    expect(plural(1, 'file')).toBe('1 file')
    expect(plural(3000, 'file')).toBe('3,000 files')
    expect(formatCountdown(581)).toBe('9:41')
    expect(formatCountdown(-3)).toBe('0:00')
  })

  it('relative times', () => {
    const now = Date.parse('2026-11-24T12:00:00.000Z')
    expect(formatWhen('2026-11-24T11:59:40.000Z', now)).toBe('Just now')
    expect(formatWhen('2026-11-24T11:56:00.000Z', now)).toBe('4 minutes ago')
    expect(formatWhen('2026-11-24T11:59:00.000Z', now)).toBe('1 minute ago')
    expect(formatWhen(null, now)).toBe('')
    expect(formatWhen('nonsense', now)).toBe('')
    expect(formatWhen('2026-11-20T10:00:00.000Z', now)).toMatch(/at/)
    expect(secondsUntil('2026-11-24T12:01:00.000Z', now)).toBe(60)
    expect(secondsUntil('2026-11-24T11:00:00.000Z', now)).toBe(0)
  })

  it('paths', () => {
    expect(driveFolderOf('/Projects/Tower/Level 3.rvt')).toBe('Projects / Tower')
    expect(driveFolderOf('/a.txt')).toBe('Top of the drive')
    expect(baseName('D:\\Exports\\Tower')).toBe('Tower')
    expect(baseName('/home/x/y/')).toBe('y')
  })
})
