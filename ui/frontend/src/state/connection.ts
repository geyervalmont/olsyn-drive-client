// Keeps one event stream open to the agent, feeds the store and lets screens
// listen to raw events (the transfer lists need every transfer change).
// Reconnects with backoff 0.25 s doubling to 5 s, resuming with Last-Event-ID.

import { AgentBlockedError } from '../api/errors'
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

  /**
   * Reads the agent's state now (the window came to the front). The event
   * stream keeps the window current; this covers a stream that went quiet
   * without an error, so what the window shows is never older than its last
   * activation.
   */
  refresh() {
    if (this.stopped) return
    const stream = this.store.getState().stream
    if (stream === 'reconnecting' || stream === 'unreachable') {
      this.retryNow()
      return
    }
    this.client
      .state()
      .then((state) => {
        if (!this.stopped) this.store.dispatch({ type: 'state', state })
      })
      .catch(() => undefined)
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
      onError: (err) => {
        this.close = null
        if (this.stopped) return
        this.failures += 1
        const blocked = err instanceof AgentBlockedError ? { title: err.title, detail: err.detail, helpUrl: err.helpUrl } : null
        if (blocked || this.store.getState().blocked) this.store.dispatch({ type: 'blocked', blocked })
        this.store.dispatch({ type: 'stream', status: blocked || this.failures >= UNREACHABLE_AFTER ? 'unreachable' : 'reconnecting' })
        const delay = Math.min(BACKOFF_MAX_MS, BACKOFF_START_MS * 2 ** Math.max(0, this.failures - 1))
        this.timer = setTimeout(() => {
          this.timer = null
          if (!this.stopped) this.open()
        }, delay)
      },
    })
  }
}
