import { createContext, useCallback, useContext, useEffect, useRef, useState, useSyncExternalStore, type ReactNode } from 'react'
import type { AgentClient } from '../api/client'
import { AgentError, AgentUnreachableError } from '../api/errors'
import type { Shell } from '../lib/shell'
import type { AgentConnection } from './connection'
import type { AgentView } from './reducer'

export interface AgentContextValue {
  client: AgentClient
  connection: AgentConnection
  shell: Shell
}

const Ctx = createContext<AgentContextValue | null>(null)

export function AgentProvider({ value, children }: { value: AgentContextValue; children: ReactNode }) {
  useEffect(() => {
    value.connection.start()
    return () => value.connection.stop()
  }, [value.connection])
  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useAgentContext(): AgentContextValue {
  const value = useContext(Ctx)
  if (!value) throw new Error('useAgentContext outside AgentProvider')
  return value
}

export function useAgentView(): AgentView {
  const { connection } = useAgentContext()
  const store = connection.store
  return useSyncExternalStore(store.subscribe, store.getState, store.getState)
}

export function useClient(): AgentClient {
  return useAgentContext().client
}

export function useShell(): Shell {
  return useAgentContext().shell
}

/** A readable sentence for any error an action can throw. */
export function errorText(err: unknown): string {
  if (err instanceof AgentError) return err.message
  if (err instanceof AgentUnreachableError) return err.message
  if (err instanceof Error && err.message) return err.message
  return 'Something went wrong. Try again.'
}

/** True when Olsyn Drive itself did not answer (not running yet, or starting), as opposed to an answer saying no. */
export function unreachableError(err: unknown): boolean {
  return err instanceof AgentUnreachableError || (err instanceof AgentError && err.code === 'agent_unreachable')
}

/**
 * Runs agent calls for a button: tracks which one is pending (so it can show
 * its pending state at once) and the last error in words.
 */
export function useAction() {
  const [pending, setPending] = useState<string | null>(null)
  const [error, setError] = useState<{ key: string; message: string; code: string | null; unreachable: boolean } | null>(null)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])
  const run = useCallback(async <T,>(key: string, fn: () => Promise<T>): Promise<T | undefined> => {
    setPending(key)
    setError(null)
    try {
      return await fn()
    } catch (err) {
      if (mounted.current)
        setError({ key, message: errorText(err), code: err instanceof AgentError ? err.code : null, unreachable: unreachableError(err) })
      return undefined
    } finally {
      if (mounted.current) setPending((p) => (p === key ? null : p))
    }
  }, [])
  const clearError = useCallback(() => setError(null), [])
  return { pending, error, run, clearError }
}
