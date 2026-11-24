// One tab of the Transfers screen: pages from GET /v1/transfers, then kept live
// from transfer and held events. Updates are applied once per frame so a busy
// copy does not re-render the list for every event.

import { useCallback, useEffect, useRef, useState } from 'react'
import type { IncomingEvent, ListState, Transfer, TransferCounts } from '../api/types'
import { errorText, useAgentContext, useAgentView } from './context'

export const PAGE_SIZE = 200

export interface TransferListState {
  items: Transfer[]
  next: string | null
  counts: TransferCounts | null
  loading: boolean
  loadingMore: boolean
  error: string | null
}

const finished = new Set(['done', 'skipped', 'failed', 'held', 'cancelled'])

/** Applies one transfer change to a tab's items. Pure, exported for tests. */
export function applyToList(items: Transfer[], t: Transfer, states: ReadonlySet<string>, complete: boolean, removed = false): Transfer[] {
  const index = items.findIndex((x) => x.id === t.id)
  const belongs = !removed && states.has(t.state)
  if (index >= 0) {
    if (!belongs) return items.slice(0, index).concat(items.slice(index + 1))
    const next = items.slice()
    next[index] = t
    return next
  }
  if (!belongs) return items
  // Newest first for finished and held; queue order (end) for waiting, front for active.
  if (finished.has(t.state)) return [t, ...items]
  if (t.state === 'active') return [t, ...items]
  return complete ? [...items, t] : items
}

export function useTransferList(states: ListState[]) {
  const { client, connection } = useAgentContext()
  const generation = useAgentView().generation
  const key = states.join(',')
  const [list, setList] = useState<TransferListState>({ items: [], next: null, counts: null, loading: true, loadingMore: false, error: null })
  const pending = useRef<{ t: Transfer; removed: boolean }[]>([])
  const frame = useRef<number | null>(null)
  const request = useRef(0)

  const load = useCallback(() => {
    const id = ++request.current
    setList((l) => ({ ...l, loading: true, error: null }))
    client
      .transfers({ state: states, limit: PAGE_SIZE })
      .then((page) => {
        if (id !== request.current) return
        pending.current = []
        setList({ items: page.items, next: page.next_cursor, counts: page.counts, loading: false, loadingMore: false, error: null })
      })
      .catch((err: unknown) => {
        if (id !== request.current) return
        setList((l) => ({ ...l, loading: false, error: errorText(err) }))
      })
    // states is represented by key
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, key])

  useEffect(() => {
    load()
  }, [load, generation])

  const current = useRef(list)
  current.current = list
  const loadMore = useCallback(() => {
    const l = current.current
    if (!l.next || l.loadingMore || l.loading) return
    const id = request.current
    const cursor = l.next
    current.current = { ...l, loadingMore: true }
    setList((cur) => ({ ...cur, loadingMore: true }))
    client
      .transfers({ state: states, cursor, limit: PAGE_SIZE })
      .then((page) => {
        if (id !== request.current) return
        setList((cur) => {
          const known = new Set(cur.items.map((x) => x.id))
          return { ...cur, items: cur.items.concat(page.items.filter((x) => !known.has(x.id))), next: page.next_cursor, counts: page.counts, loadingMore: false }
        })
      })
      .catch(() => setList((cur) => ({ ...cur, loadingMore: false })))
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [client, key])

  useEffect(() => {
    const set = new Set<string>(states)
    const flush = () => {
      frame.current = null
      const batch = pending.current
      pending.current = []
      if (batch.length === 0) return
      const at = Date.now()
      setList((l) => {
        let items = l.items
        for (const { t, removed } of batch) items = applyToList(items, { ...t, receivedAt: at } as Transfer, set, l.next === null, removed)
        return items === l.items ? l : { ...l, items }
      })
    }
    const schedule = () => {
      if (frame.current !== null) return
      frame.current = typeof requestAnimationFrame === 'function' ? requestAnimationFrame(flush) : (setTimeout(flush, 16) as unknown as number)
    }
    const off = connection.onEvent((event: IncomingEvent) => {
      if (event.event === 'transfer') pending.current.push({ t: event.data, removed: false })
      else if (event.event === 'held') pending.current.push({ t: event.data.item, removed: event.data.op === 'removed' })
      else if (event.event === 'snapshot') {
        load()
        return
      } else return
      schedule()
    })
    return () => {
      off()
      if (frame.current !== null) {
        if (typeof cancelAnimationFrame === 'function') cancelAnimationFrame(frame.current)
        clearTimeout(frame.current)
      }
      frame.current = null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [connection, key, load])

  return { ...list, reload: load, loadMore }
}
