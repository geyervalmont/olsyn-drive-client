// A tiny external store for React's useSyncExternalStore. Listeners are told
// once per microtask, so a burst of events from one stream chunk renders once.

import { initialView, reduce, type Action, type AgentView } from './reducer'

export interface Store {
  getState(): AgentView
  dispatch(action: Action): void
  subscribe(listener: () => void): () => void
}

export function createStore(initial: AgentView = initialView): Store {
  let state = initial
  const listeners = new Set<() => void>()
  let scheduled = false

  const notify = () => {
    scheduled = false
    for (const l of [...listeners]) l()
  }

  return {
    getState: () => state,
    dispatch(action) {
      const next = reduce(state, action)
      if (next === state) return
      state = next
      if (!scheduled) {
        scheduled = true
        queueMicrotask(notify)
      }
    },
    subscribe(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
}
