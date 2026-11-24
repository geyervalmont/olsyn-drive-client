import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AgentClient } from '../api/client'
import type { StreamHandlers, Transport } from '../api/transport'
import { state } from '../test/fixtures'
import { AgentConnection, UNREACHABLE_AFTER } from './connection'
import { createStore } from './store'

class ScriptedTransport implements Transport {
  opens: (string | null)[] = []
  handlers: StreamHandlers | null = null
  async request() {
    return { status: 200, body: {} }
  }
  stream(lastEventId: string | null, handlers: StreamHandlers) {
    this.opens.push(lastEventId)
    this.handlers = handlers
    return () => undefined
  }
}

describe('AgentConnection', () => {
  beforeEach(() => vi.useFakeTimers())
  afterEach(() => vi.useRealTimers())

  it('reconnects with backoff, resumes from the last id and says when the agent is unreachable', () => {
    const t = new ScriptedTransport()
    const store = createStore()
    const conn = new AgentConnection(new AgentClient(t), store, () => 5)
    conn.start()
    expect(t.opens).toEqual([null])
    t.handlers!.onMessage({ id: 'r1-1', event: 'snapshot', data: JSON.stringify({ state: state(), active: [], held: [], lagged: false }) })
    expect(store.getState().stream).toBe('live')
    expect(store.getState().lastEventId).toBe('r1-1')

    // Fail repeatedly: 250, 500, 1000, 2000 ms ...
    const delays = [250, 500, 1000, 2000, 4000, 5000, 5000]
    for (let i = 0; i < delays.length; i++) {
      t.handlers!.onError(new Error('gone'))
      expect(store.getState().stream).toBe(i + 1 >= UNREACHABLE_AFTER ? 'unreachable' : 'reconnecting')
      const before = t.opens.length
      vi.advanceTimersByTime(delays[i] - 1)
      expect(t.opens.length).toBe(before)
      vi.advanceTimersByTime(1)
      expect(t.opens.length).toBe(before + 1)
      expect(t.opens.at(-1)).toBe('r1-1')
    }
    // A message resets the failure count.
    t.handlers!.onMessage({ id: 'r1-2', event: 'summary', data: JSON.stringify(state().transfers) })
    expect(store.getState().stream).toBe('live')
    t.handlers!.onError(new Error('again'))
    expect(store.getState().stream).toBe('reconnecting')
    conn.stop()
  })

  it('passes raw events to listeners', () => {
    const t = new ScriptedTransport()
    const conn = new AgentConnection(new AgentClient(t), createStore())
    const seen: string[] = []
    conn.onEvent((e) => seen.push(e.event))
    conn.start()
    t.handlers!.onMessage({ id: 'r1-1', event: 'notification', data: JSON.stringify({ id: 'n', kind: 'finished', title: 't', body: '', at: '', action: null }) })
    expect(seen).toEqual(['notification'])
    conn.stop()
  })
})
