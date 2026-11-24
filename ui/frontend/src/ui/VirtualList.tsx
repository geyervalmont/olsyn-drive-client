// A keyboard-navigable list that only renders the rows on screen once it is
// long (12,000 rows stay smooth). Short lists render plainly, which also keeps
// them testable without layout.

import { useVirtualizer } from '@tanstack/react-virtual'
import { useCallback, useEffect, useRef, type KeyboardEvent, type ReactNode } from 'react'

export const VIRTUALISE_ABOVE = 60

export function VirtualList<T>({
  items,
  rowHeight,
  getKey,
  renderRow,
  label,
  onEndReached,
  footer,
  id,
  labelledBy,
}: {
  items: T[]
  rowHeight: number
  getKey: (item: T) => string
  renderRow: (item: T, index: number) => ReactNode
  label?: string
  labelledBy?: string
  onEndReached?: () => void
  footer?: ReactNode
  id?: string
}) {
  const scrollRef = useRef<HTMLDivElement>(null)
  const virtual = items.length > VIRTUALISE_ABOVE
  const virtualizer = useVirtualizer({
    count: virtual ? items.length : 0,
    getScrollElement: () => scrollRef.current,
    estimateSize: () => rowHeight,
    overscan: 8,
    getItemKey: (index) => getKey(items[index]),
  })

  const lastVisible = virtual ? virtualizer.getVirtualItems().at(-1)?.index ?? 0 : items.length - 1
  const endRef = useRef(onEndReached)
  endRef.current = onEndReached
  useEffect(() => {
    if (lastVisible >= items.length - 20) endRef.current?.()
  }, [lastVisible, items.length])

  const focusRow = useCallback(
    (index: number) => {
      const clamped = Math.max(0, Math.min(items.length - 1, index))
      if (virtual) virtualizer.scrollToIndex(clamped, { align: 'auto' })
      requestAnimationFrame(() => {
        scrollRef.current?.querySelector<HTMLElement>('[data-row-index="' + clamped + '"]')?.focus()
      })
    },
    [items.length, virtual, virtualizer],
  )

  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const row = (e.target as HTMLElement).closest<HTMLElement>('[data-row-index]')
    const current = row ? Number(row.dataset.rowIndex) : -1
    const page = Math.max(1, Math.floor((scrollRef.current?.clientHeight ?? rowHeight * 8) / rowHeight) - 1)
    let next: number | null = null
    if (e.key === 'ArrowDown') next = current + 1
    else if (e.key === 'ArrowUp') next = current <= 0 ? 0 : current - 1
    else if (e.key === 'PageDown') next = current + page
    else if (e.key === 'PageUp') next = current - page
    else if (e.key === 'Home' && (e.target as HTMLElement) === row) next = 0
    else if (e.key === 'End' && (e.target as HTMLElement) === row) next = items.length - 1
    if (next === null || items.length === 0) return
    // Arrow keys inside a row's buttons still move between rows.
    e.preventDefault()
    focusRow(next)
  }

  const row = (item: T, index: number) => (
    <div
      role="listitem"
      aria-setsize={items.length}
      aria-posinset={index + 1}
      tabIndex={index === 0 ? 0 : -1}
      data-row-index={index}
      className="vrow"
      style={{ minHeight: rowHeight }}
    >
      {renderRow(item, index)}
    </div>
  )

  return (
    <div ref={scrollRef} className="vlist" onKeyDown={onKeyDown} id={id}>
      <div role="list" aria-label={label} aria-labelledby={labelledBy}>
        {virtual ? (
          <div style={{ height: virtualizer.getTotalSize(), position: 'relative' }}>
            {virtualizer.getVirtualItems().map((v) => (
              <div key={v.key} style={{ position: 'absolute', top: 0, left: 0, right: 0, height: rowHeight, transform: 'translateY(' + v.start + 'px)' }}>
                {row(items[v.index], v.index)}
              </div>
            ))}
          </div>
        ) : (
          items.map((item, index) => <div key={getKey(item)}>{row(item, index)}</div>)
        )}
      </div>
      {footer}
    </div>
  )
}
