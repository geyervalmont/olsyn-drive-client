// ARIA tabs: arrow keys move between tabs, Home and End jump.

import { useRef, type KeyboardEvent } from 'react'

export interface TabItem<K extends string> {
  key: K
  label: string
  count?: number
  tone?: 'warn'
}

export function Tabs<K extends string>({
  items,
  value,
  onChange,
  label,
  idPrefix,
}: {
  items: TabItem<K>[]
  value: K
  onChange: (key: K) => void
  label: string
  idPrefix: string
}) {
  const refs = useRef(new Map<K, HTMLButtonElement>())
  const onKeyDown = (e: KeyboardEvent<HTMLDivElement>) => {
    const index = items.findIndex((i) => i.key === value)
    let next = index
    if (e.key === 'ArrowRight') next = (index + 1) % items.length
    else if (e.key === 'ArrowLeft') next = (index - 1 + items.length) % items.length
    else if (e.key === 'Home') next = 0
    else if (e.key === 'End') next = items.length - 1
    else return
    e.preventDefault()
    const key = items[next].key
    onChange(key)
    refs.current.get(key)?.focus()
  }
  return (
    <div className="tabs" role="tablist" aria-label={label} onKeyDown={onKeyDown}>
      {items.map((item) => {
        const selected = item.key === value
        return (
          <button
            key={item.key}
            ref={(node) => {
              if (node) refs.current.set(item.key, node)
              else refs.current.delete(item.key)
            }}
            type="button"
            role="tab"
            id={idPrefix + '-tab-' + item.key}
            aria-controls={idPrefix + '-panel'}
            aria-selected={selected}
            tabIndex={selected ? 0 : -1}
            className={'tab' + (selected ? ' is-selected' : '') + (item.tone === 'warn' && item.count ? ' tone-warn' : '')}
            onClick={() => onChange(item.key)}
          >
            <span>{item.label}</span>
            {item.count !== undefined && <span className="tab-count">{item.count.toLocaleString('en')}</span>}
          </button>
        )
      })}
    </div>
  )
}
