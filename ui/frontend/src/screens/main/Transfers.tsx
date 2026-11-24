import { useEffect, useId, useRef, useState, type KeyboardEvent } from 'react'
import type { ListState, TransferCounts } from '../../api/types'
import { formatBytes, formatSpeed, formatTimeLeft, plural } from '../../lib/format'
import { useNow } from '../../lib/useNow'
import { useAction, useAgentContext, useAgentView } from '../../state/context'
import { useTransferList } from '../../state/useTransferList'
import { Icon } from '../../ui/Icon'
import { ProgressBar, Spinner } from '../../ui/Progress'
import { Tabs, type TabItem } from '../../ui/Tabs'
import { VirtualList } from '../../ui/VirtualList'
import { Button, EmptyState, InlineError } from '../../ui/controls'
import { HeldList } from './HeldList'
import { ROW_HEIGHT, TransferRow } from './TransferRow'

export type TransferTab = 'active' | 'waiting' | 'held' | 'done' | 'skipped' | 'failed'

export const transferTabs: TransferTab[] = ['active', 'waiting', 'held', 'done', 'skipped', 'failed']

const empty: Record<Exclude<TransferTab, 'held'>, { title: string; body: string }> = {
  active: { title: 'Nothing is uploading right now.', body: 'Files you save on the drive letter, or send from here, show their progress in this tab.' },
  waiting: { title: 'Nothing is waiting.', body: 'When several files are saved at once, the ones not yet started line up here.' },
  done: { title: 'Nothing uploaded yet.', body: 'Files show up here once the drive has them.' },
  skipped: { title: 'Nothing skipped.', body: 'Files that are already on the drive, and Windows system files like Thumbs.db, are listed here instead of being sent twice.' },
  failed: { title: 'Nothing failed.', body: 'Files that could not be sent after several tries show up here with a Try again button.' },
}

export function Transfers({ tab, onTab, onSend }: { tab: TransferTab; onTab: (tab: TransferTab) => void; onSend: () => void }) {
  const view = useAgentView()
  const { connection } = useAgentContext()
  const summary = view.state?.transfers
  const [counts, setCounts] = useState<TransferCounts | null>(null)
  const seen = useRef(new Set<string>())

  // Finished counts: from the last page, plus finishes seen since.
  useEffect(() => {
    return connection.onEvent((e) => {
      if (e.event !== 'transfer') return
      const s = e.data.state
      if ((s === 'done' || s === 'skipped' || s === 'failed') && !seen.current.has(e.data.id)) {
        seen.current.add(e.data.id)
        setCounts((c) => (c ? { ...c, [s]: c[s] + 1 } : c))
      }
    })
  }, [connection])

  const items: TabItem<TransferTab>[] = [
    { key: 'active', label: 'Sending', count: summary?.active ?? counts?.active },
    { key: 'waiting', label: 'Waiting', count: summary?.waiting ?? counts?.waiting },
    { key: 'held', label: 'Held', count: view.held.length, tone: 'warn' },
    { key: 'done', label: 'Done', count: counts?.done },
    { key: 'skipped', label: 'Already there', count: counts?.skipped },
  ]
  if ((counts?.failed ?? 0) > 0 || tab === 'failed') items.push({ key: 'failed', label: 'Not uploaded', count: counts?.failed })

  const titleId = useId()
  return (
    <section className="screen" aria-labelledby={titleId}>
      <header className="screen-head">
        <h1 id={titleId} className="screen-title display">
          Transfers
        </h1>
        <Button variant="secondary" icon="send" onClick={onSend}>
          Send a folder…
        </Button>
      </header>
      <SummaryStrip />
      <Tabs items={items} value={tab} onChange={onTab} label="Transfers" idPrefix="transfers" />
      <div className="tab-panel" role="tabpanel" id="transfers-panel" aria-labelledby={'transfers-tab-' + tab}>
        {tab === 'held' ? <HeldList items={view.held} labelledBy={'transfers-tab-held'} /> : <TransferTabList key={tab} tab={tab} onCounts={setCounts} />}
      </div>
    </section>
  )
}

function TransferTabList({ tab, onCounts }: { tab: Exclude<TransferTab, 'held'>; onCounts: (c: TransferCounts) => void }) {
  const states: ListState[] = [tab]
  const list = useTransferList(states)
  const hasActive = tab === 'active' && list.items.length > 0
  const now = useNow(hasActive ? 200 : 15_000)
  useEffect(() => {
    if (list.counts) onCounts(list.counts)
  }, [list.counts, onCounts])

  if (list.loading && list.items.length === 0) return <ListSkeleton />
  if (list.error && list.items.length === 0) return <InlineError message={list.error} onRetry={list.reload} />
  if (list.items.length === 0) {
    const e = empty[tab]
    return <EmptyState icon={tab === 'done' ? 'check' : tab === 'skipped' ? 'skip' : tab === 'failed' ? 'alert' : 'transfers'} title={e.title}>{e.body}</EmptyState>
  }
  return (
    <VirtualList
      items={list.items}
      rowHeight={ROW_HEIGHT}
      getKey={(t) => t.id}
      labelledBy={'transfers-tab-' + tab}
      onEndReached={list.loadMore}
      renderRow={(t) => <TransferRow t={t} now={now} />}
      footer={
        list.loadingMore ? (
          <p className="list-foot">
            <Spinner size={14} /> Loading more
          </p>
        ) : null
      }
    />
  )
}

function ListSkeleton() {
  return (
    <div className="skeleton" aria-busy="true" aria-label="Loading">
      {Array.from({ length: 6 }, (_, i) => (
        <div key={i} className="skeleton-row" style={{ opacity: 1 - i * 0.14 }}>
          <span className="sk sk-icon" />
          <span className="sk sk-line" style={{ width: 40 + ((i * 17) % 35) + '%' }} />
          <span className="sk sk-bar" />
        </div>
      ))}
    </div>
  )
}

export function SummaryStrip() {
  const view = useAgentView()
  const state = view.state
  if (!state) return null
  const s = state.transfers
  const moving = s.files_left > 0
  const progress = s.bytes_total > 0 ? s.bytes_done / s.bytes_total : null
  const eta = formatTimeLeft(s.eta_seconds)
  return (
    <div className={'summary' + (moving ? '' : ' is-idle')}>
      <div className="summary-text">
        {moving ? (
          <>
            <p className="summary-title">
              {state.paused ? 'Paused with ' + plural(s.files_left, 'file') + ' to go' : (s.up_bps > 0 || s.down_bps === 0 ? 'Sending ' : 'Fetching ') + plural(s.files_left, 'file')}
            </p>
            <p className="summary-meta tnum">
              <span>{formatBytes(Math.max(0, s.bytes_total - s.bytes_done)) + ' to go'}</span>
              {!state.paused && s.up_bps > 0 && <span>{formatSpeed(s.up_bps)}</span>}
              {!state.paused && eta && <span>{eta}</span>}
            </p>
          </>
        ) : (
          <>
            <p className="summary-title">All changes are uploaded</p>
            <p className="summary-meta">Nothing saved on {state.mount.letter ?? 'the drive letter'} is waiting to upload.</p>
          </>
        )}
      </div>
      {moving && (
        <div className="summary-bar">
          <ProgressBar value={progress} label="Overall progress" tone={state.paused ? 'paused' : 'busy'} valueText={progress === null ? undefined : Math.round(progress * 100) + ' percent'} />
        </div>
      )}
      <PauseControl />
    </div>
  )
}

const pauseChoices: { label: string; minutes?: number }[] = [
  { label: 'For 30 minutes', minutes: 30 },
  { label: 'For 1 hour', minutes: 60 },
  { label: 'For 4 hours', minutes: 240 },
  { label: 'Until I resume' },
]

export function PauseControl({ compact = false }: { compact?: boolean }) {
  const view = useAgentView()
  const { client } = useAgentContext()
  const { pending, error, run } = useAction()
  const [open, setOpen] = useState(false)
  const menuId = useId()
  const wrap = useRef<HTMLDivElement>(null)
  const state = view.state
  useEffect(() => {
    if (!open) return
    const onDown = (e: MouseEvent) => {
      if (!wrap.current?.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', onDown)
    wrap.current?.querySelector<HTMLElement>('[role="menuitem"]')?.focus()
    return () => document.removeEventListener('mousedown', onDown)
  }, [open])
  if (!state?.account) return null
  if (state.paused) {
    return (
      <Button size={compact ? 's' : 'm'} variant="primary" icon="play" pending={pending === 'resume'} onClick={() => void run('resume', () => client.resume())}>
        Resume
      </Button>
    )
  }
  const onMenuKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const items = Array.from(wrap.current?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])
    const i = items.indexOf(document.activeElement as HTMLElement)
    if (e.key === 'Escape') {
      setOpen(false)
      wrap.current?.querySelector<HTMLElement>('[aria-haspopup]')?.focus()
    } else if (e.key === 'ArrowDown') {
      e.preventDefault()
      items[(i + 1) % items.length]?.focus()
    } else if (e.key === 'ArrowUp') {
      e.preventDefault()
      items[(i - 1 + items.length) % items.length]?.focus()
    }
  }
  return (
    <div className="menu-wrap" ref={wrap} onKeyDown={onMenuKey}>
      <button type="button" className={'btn btn-' + (compact ? 's' : 'm') + ' btn-secondary'} aria-haspopup="menu" aria-expanded={open} aria-controls={menuId} onClick={() => setOpen((o) => !o)} disabled={pending === 'pause'}>
        {pending === 'pause' ? <Spinner size={14} /> : <Icon name="pause" size={compact ? 15 : 17} />}
        <span>Pause</span>
      </button>
      {open && (
        <div className="menu" role="menu" id={menuId} aria-label="Pause uploads">
          {pauseChoices.map((c) => (
            <button
              key={c.label}
              type="button"
              role="menuitem"
              className="menu-item"
              onClick={() => {
                setOpen(false)
                void run('pause', () => client.pause(c.minutes))
              }}
            >
              {c.label}
            </button>
          ))}
        </div>
      )}
      {error && <p className="field-error" role="alert">{error.message}</p>}
    </div>
  )
}
