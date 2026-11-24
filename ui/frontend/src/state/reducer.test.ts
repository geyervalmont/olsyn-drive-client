import { describe, expect, it } from 'vitest'
import type { IncomingEvent } from '../api/types'
import { state, summary, transfer } from '../test/fixtures'
import { initialView, reduce, RECENT_LIMIT, type AgentView } from './reducer'
import { statusLine } from './derive'
import { formatBytes } from '../lib/format'
import { applyToList } from './useTransferList'

const ev = (e: Omit<IncomingEvent, 'id'> & { id?: string }): { type: 'event'; event: IncomingEvent; at: number } => ({
  type: 'event',
  event: { id: e.id ?? null, ...e } as IncomingEvent,
  at: 1000,
})

const snap = (over: Parameters<typeof state>[0] = {}, active = [transfer('a', { state: 'active' }), transfer('b')], held = [transfer('h', { state: 'held' })]): AgentView =>
  reduce(initialView, ev({ id: 'r1-1', event: 'snapshot', data: { state: state(over), active, held, lagged: false } }))

describe('reducer', () => {
  it('keeps one entry per send batch, newest first', () => {
    const batch = (id: string, done: number) => ({
      batch_id: id, state: 'sending', drive_folder: '/Projects', local_paths: ['D:\\Exports'], files: 10, files_done: done,
      files_skipped: 0, files_failed: 0, files_held: 0, files_not_local: 0, bytes: 100, bytes_done: done * 10, bps: 0,
      eta_seconds: null, started_at: '2026-11-24T00:00:00Z', finished_at: null, error: null,
    })
    let v = snap()
    v = reduce(v, ev({ event: 'batch', data: batch('b_1', 1) }))
    v = reduce(v, ev({ event: 'batch', data: batch('b_2', 0) }))
    v = reduce(v, ev({ event: 'batch', data: batch('b_1', 5) }))
    expect(v.batches.map((b) => [b.batch_id, b.files_done])).toEqual([['b_2', 0], ['b_1', 5]])
  })

  it('builds the view from a snapshot', () => {
    const v = snap()
    expect(v.stream).toBe('live')
    expect(v.state?.connection).toBe('connected')
    expect(v.live.map((t) => t.id)).toEqual(['a', 'b'])
    expect(v.live[0].receivedAt).toBe(1000)
    expect(v.held.map((t) => t.id)).toEqual(['h'])
    expect(v.lastEventId).toBe('r1-1')
  })

  it('applies summary, signin, update and settings events into the state', () => {
    let v = snap()
    v = reduce(v, ev({ event: 'summary', data: summary({ active: 3, files_left: 9 }) }))
    expect(v.state?.transfers.files_left).toBe(9)
    v = reduce(v, ev({ event: 'signin', data: { state: 'waiting', user_code: 'BCDF-GHJK', verification_url: 'x', expires_at: 'y', error: null } }))
    expect(v.state?.signin?.user_code).toBe('BCDF-GHJK')
    v = reduce(v, ev({ event: 'update', data: { ...v.state!.update, state: 'available', available: '1.5.0' } }))
    expect(v.state?.update.available).toBe('1.5.0')
  })

  it('moves a finished transfer from live to recent, newest first, capped', () => {
    let v = snap()
    v = reduce(v, ev({ event: 'transfer', data: transfer('a', { state: 'done' }) }))
    expect(v.live.map((t) => t.id)).toEqual(['b'])
    expect(v.recent[0].id).toBe('a')
    for (let i = 0; i < RECENT_LIMIT + 5; i++) v = reduce(v, ev({ event: 'transfer', data: transfer('d' + i, { state: 'done' }) }))
    expect(v.recent).toHaveLength(RECENT_LIMIT)
    expect(v.recent[0].id).toBe('d' + (RECENT_LIMIT + 4))
  })

  it('updates live progress in place and adds new active work at the front', () => {
    let v = snap()
    v = reduce(v, ev({ event: 'transfer', data: transfer('b', { state: 'active', bytes_done: 500, bps: 100 }) }))
    expect(v.live.find((t) => t.id === 'b')?.bytes_done).toBe(500)
    v = reduce(v, ev({ event: 'transfer', data: transfer('c', { state: 'active' }) }))
    expect(v.live[0].id).toBe('c')
  })

  it('adds and removes held items', () => {
    let v = snap()
    v = reduce(v, ev({ event: 'held', data: { op: 'added', item: transfer('b', { state: 'held' }) } }))
    expect(v.held.map((t) => t.id)).toEqual(['b', 'h'])
    expect(v.live.some((t) => t.id === 'b')).toBe(false)
    v = reduce(v, ev({ event: 'held', data: { op: 'removed', item: transfer('h') } }))
    expect(v.held.map((t) => t.id)).toEqual(['b'])
  })

  it('keeps notifications newest first without duplicates', () => {
    let v = snap()
    const n = { id: 'n1', kind: 'finished', title: 'Done', body: '', at: 'x', action: null }
    v = reduce(v, ev({ event: 'notification', data: n }))
    v = reduce(v, ev({ event: 'notification', data: n }))
    expect(v.notifications).toHaveLength(1)
  })

  it('shows an unknown connection as attention', () => {
    const v = snap({ connection: 'something_new' as never })
    expect(v.state?.connection).toBe('attention')
  })

  it('bumps the generation when the agent restarts', () => {
    let v = snap()
    expect(v.generation).toBe(0)
    v = reduce(v, ev({ event: 'state', data: state({ agent_run: 'r2', version: '1.5.0' }) }))
    expect(v.generation).toBe(1)
    expect(v.state?.version).toBe('1.5.0')
  })

  it('tracks stream status without needless changes', () => {
    const v = snap()
    expect(reduce(v, { type: 'stream', status: 'live' })).toBe(v)
    expect(reduce(v, { type: 'stream', status: 'reconnecting' }).stream).toBe('reconnecting')
  })
})

describe('statusLine', () => {
  it('says what is happening in plain words', () => {
    expect(statusLine(snap()).title).toBe('All changes are uploaded')
    const working = statusLine(snap({ connection: 'working', transfers: summary({ files_left: 16, active: 2, bytes_total: 2000, bytes_done: 500, up_bps: 100, eta_seconds: 58 }) }))
    expect(working.title).toBe('Uploading 16 files · ' + formatBytes(1500) + ' left') // an agent without files_done
    expect(working.detail).toBe('About 1 minute left.')
    expect(working.progress).toBe(0.25)
    const counted = statusLine(snap({ connection: 'working', transfers: summary({ files_left: 118, files_done: 2, bytes_total: 1.4e9, bytes_done: 0.2e9 }) }))
    expect(counted.title).toBe('Uploading 3 of 120 · ' + formatBytes(1.2e9) + ' left')
    expect(statusLine(snap({ connection: 'offline' })).title).toBe("Can't reach the drive.")
    expect(statusLine(snap({ connection: 'signed_out', account: null, reason: { code: 'token_revoked', message: '' } })).title).toBe('Signed out by the drive.')
    expect(statusLine({ ...initialView, stream: 'unreachable' }).tone).toBe('danger')
  })
})

describe('applyToList', () => {
  const tabs = (s: string[]) => new Set(s)
  it('replaces, removes and inserts by state', () => {
    const items = [transfer('a', { state: 'waiting' }), transfer('b', { state: 'waiting' })]
    // a starts: leaves the waiting tab.
    const w = applyToList(items, transfer('a', { state: 'active' }), tabs(['waiting']), true)
    expect(w.map((t) => t.id)).toEqual(['b'])
    // a new waiting item joins the end only when the whole list is loaded.
    expect(applyToList(w, transfer('c'), tabs(['waiting']), true).map((t) => t.id)).toEqual(['b', 'c'])
    expect(applyToList(w, transfer('c'), tabs(['waiting']), false)).toBe(w)
    // finished items go first.
    const d = applyToList([transfer('x', { state: 'done' })], transfer('y', { state: 'done' }), tabs(['done']), false)
    expect(d.map((t) => t.id)).toEqual(['y', 'x'])
    // held removal.
    expect(applyToList([transfer('h', { state: 'held' })], transfer('h', { state: 'held' }), tabs(['held']), true, true)).toEqual([])
  })
})
