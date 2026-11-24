// The UI's picture of the agent, rebuilt from the event stream (docs/ipc.md:
// "render from the snapshot, then apply events; never poll"). Pure and small:
// the long per-tab transfer lists live in useTransferList, not here.

import type {
  AgentNotification,
  Batch,
  IncomingEvent,
  Settings,
  State,
  Transfer,
} from '../api/types'

export type StreamStatus = 'connecting' | 'live' | 'reconnecting' | 'unreachable'

/** A transfer plus the time this UI received it, for smooth progress between events. */
export type LiveTransfer = Transfer & { receivedAt: number }

export interface AgentView {
  stream: StreamStatus
  state: State | null
  /** Active and waiting transfers, in queue order (the snapshot's first 50 plus what events add). */
  live: LiveTransfer[]
  /** Recently finished, newest first, for the popover. */
  recent: Transfer[]
  held: Transfer[]
  settings: Settings | null
  notifications: AgentNotification[]
  /** Sends from this PC (POST /v1/send), newest first. */
  batches: Batch[]
  lastEventId: string | null
  /** Bumped when the agent restarts (a new agent_run): screens re-read what they fetched. */
  generation: number
}

export const LIVE_LIMIT = 200
export const RECENT_LIMIT = 12
export const NOTIFICATION_LIMIT = 100
export const BATCH_LIMIT = 20

export const initialView: AgentView = {
  stream: 'connecting',
  state: null,
  live: [],
  recent: [],
  held: [],
  settings: null,
  notifications: [],
  batches: [],
  lastEventId: null,
  generation: 0,
}

export type Action =
  | { type: 'stream'; status: StreamStatus }
  | { type: 'event'; event: IncomingEvent; at: number }
  | { type: 'settings'; settings: Settings }
  | { type: 'state'; state: State }

const isQueued = (t: Transfer) => t.state === 'active' || t.state === 'waiting'
const isFinished = (t: Transfer) => t.state === 'done' || t.state === 'skipped' || t.state === 'failed' || t.state === 'cancelled'

function applyTransfer(view: AgentView, t: Transfer, at: number): AgentView {
  const index = view.live.findIndex((x) => x.id === t.id)
  let live = view.live
  let recent = view.recent
  if (isQueued(t)) {
    const next: LiveTransfer = { ...t, receivedAt: at }
    if (index >= 0) {
      live = live.slice()
      live[index] = next
    } else if (live.length < LIVE_LIMIT) {
      live = t.state === 'active' ? [next, ...live] : [...live, next]
    }
  } else if (index >= 0) {
    live = live.filter((x) => x.id !== t.id)
  }
  if (isFinished(t)) {
    recent = [t, ...view.recent.filter((x) => x.id !== t.id)].slice(0, RECENT_LIMIT)
  }
  let held = view.held
  const heldIndex = held.findIndex((x) => x.id === t.id)
  if (heldIndex >= 0) {
    held = t.state === 'held' ? held.map((x) => (x.id === t.id ? t : x)) : held.filter((x) => x.id !== t.id)
  }
  return { ...view, live, recent, held }
}

/** Unknown connection values are shown as attention (docs/ipc.md "Versioning"). */
const knownConnections = new Set([
  'signed_out', 'linking', 'starting', 'offline', 'server_restarting', 'attention', 'paused', 'working', 'connected',
])
function normaliseState(state: State): State {
  if (knownConnections.has(state.connection)) return state
  return { ...state, connection: 'attention' }
}

function withState(view: AgentView, state: State): AgentView {
  const next = normaliseState(state)
  const restarted = view.state !== null && view.state.agent_run !== next.agent_run
  return { ...view, state: next, generation: restarted ? view.generation + 1 : view.generation }
}

export function reduce(view: AgentView, action: Action): AgentView {
  switch (action.type) {
    case 'stream':
      return view.stream === action.status ? view : { ...view, stream: action.status }
    case 'settings':
      return { ...view, settings: action.settings }
    case 'state':
      return withState(view, action.state)
    case 'event':
      return reduceEvent({ ...view, lastEventId: action.event.id ?? view.lastEventId }, action.event, action.at)
  }
}

function reduceEvent(view: AgentView, event: IncomingEvent, at: number): AgentView {
  switch (event.event) {
    case 'snapshot': {
      const next = withState(view, event.data.state)
      return {
        ...next,
        stream: 'live',
        live: event.data.active.filter(isQueued).map((t) => ({ ...t, receivedAt: at })),
        held: event.data.held,
      }
    }
    case 'state':
      return withState(view, event.data)
    case 'summary':
      return view.state ? { ...view, state: { ...view.state, transfers: event.data } } : view
    case 'transfer':
      return applyTransfer(view, event.data, at)
    case 'held': {
      const { op, item } = event.data
      const rest = view.held.filter((x) => x.id !== item.id)
      const held = op === 'added' ? [item, ...rest] : rest
      const live = op === 'added' ? view.live.filter((x) => x.id !== item.id) : view.live
      return { ...view, held, live }
    }
    case 'signin':
      return view.state ? { ...view, state: { ...view.state, signin: event.data } } : view
    case 'update':
      return view.state ? { ...view, state: { ...view.state, update: event.data } } : view
    case 'settings':
      return { ...view, settings: event.data }
    case 'notification': {
      if (view.notifications.some((n) => n.id === event.data.id)) return view
      return { ...view, notifications: [event.data, ...view.notifications].slice(0, NOTIFICATION_LIMIT) }
    }
    case 'batch': {
      const b = event.data
      const i = view.batches.findIndex((x) => x.batch_id === b.batch_id)
      const batches = i >= 0 ? view.batches.map((x) => (x.batch_id === b.batch_id ? b : x)) : [b, ...view.batches].slice(0, BATCH_LIMIT)
      return { ...view, batches }
    }
  }
}
