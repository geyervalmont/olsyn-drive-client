// Keeps one event stream open to the agent, feeds the store and lets screens
// listen to raw events (the transfer lists need every transfer change).
// Reconnects with backoff 0.25 s doubling to 5 s, resuming with Last-Event-ID.

import type { AgentClient } from '../api/client'
import type { IncomingEvent } from '../api/types'
import type { Store } from './store'

export const BACKOFF_START_MS = 250
export const BACKOFF_MAX_MS = 5_000
/** After this many failed attempts in a row the UI says the agent can't be reached. */
export const UNREACHABLE_AFTER = 4

type EventListener = (event: IncomingEvent) => void

export class AgentConnection {
  private close: (() => void) | null = null
  private timer: ReturnType<typeof setTimeout> | null = null
  private failures = 0
  private stopped = true
  private readonly listeners = new Set<EventListener>()

  constructor(
    readonly client: AgentClient,
    readonly store: Store,
    private readonly now: () => number = () => Date.now(),
  ) {}

  start() {
    if (!this.stopped) return
    this.stopped = false
    this.open()
  }

  stop() {
    this.stopped = true
    if (this.timer) clearTimeout(this.timer)
    this.timer = null
    this.close?.()
    this.close = null
  }

  /** Reconnect now (a "Try again" button). */
  retryNow() {
    if (this.stopped) return
    if (this.timer) clearTimeout(this.timer)
    this.timer = null
    this.close?.()
    this.close = null
    this.open()
  }

  onEvent(listener: EventListener): () => void {
    this.listeners.add(listener)
    return () => this.listeners.delete(listener)
  }

  private open() {
    const lastId = this.store.getState().lastEventId
    if (this.store.getState().stream !== 'unreachable') {
      this.store.dispatch({ type: 'stream', status: this.store.getState().state ? 'reconnecting' : 'connecting' })
    }
    this.close = this.client.subscribe(lastId, {
      onOpen: () => {
        this.failures = 0
      },
      onEvent: (event) => {
        this.failures = 0
        this.store.dispatch({ type: 'event', event, at: this.now() })
        if (event.event === 'snapshot' || this.store.getState().stream !== 'live') {
          this.store.dispatch({ type: 'stream', status: 'live' })
        }
        for (const l of [...this.listeners]) l(event)
      },
      onError: () => {
        this.close = null
        if (this.stopped) return
        this.failures += 1
        this.store.dispatch({ type: 'stream', status: this.failures >= UNREACHABLE_AFTER ? 'unreachable' : 'reconnecting' })
        const delay = Math.min(BACKOFF_MAX_MS, BACKOFF_START_MS * 2 ** Math.max(0, this.failures - 1))
        this.timer = setTimeout(() => {
          this.timer = null
          if (!this.stopped) this.open()
        }, delay)
      },
    })
  }
}
