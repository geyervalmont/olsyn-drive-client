// Files the drive refused for good. Each says what happened in plain words and
// offers what can be done: save a copy somewhere else, or discard it.

import { useState } from 'react'
import type { Transfer } from '../../api/types'
import { driveFolderOf, formatBytes, formatWhen } from '../../lib/format'
import { useAction, useAgentContext } from '../../state/context'
import { Dialog } from '../../ui/Dialog'
import { FileName, Truncate } from '../../ui/FileName'
import { Icon } from '../../ui/Icon'
import { useToast } from '../../ui/Toasts'
import { VirtualList } from '../../ui/VirtualList'
import { Button, EmptyState } from '../../ui/controls'

export const HELD_ROW_HEIGHT = 118

const reasonTitle: Record<string, string> = {
  managed: 'Managed folder',
  sealed: 'Sealed folder',
  forbidden: 'No permission to add here',
  legal_hold: 'On legal hold',
  quota_exceeded: 'Folder is full',
  invalid_name: 'Name not allowed',
}

export function heldReasonTitle(code: string): string {
  return reasonTitle[code] ?? 'Refused by the drive'
}

export function HeldList({ items, labelledBy }: { items: Transfer[]; labelledBy?: string }) {
  const [discarding, setDiscarding] = useState<Transfer | null>(null)
  if (items.length === 0) {
    return (
      <EmptyState icon="hold" title="Nothing is held.">
        If the drive refuses a file, it waits here with what happened and what you can do. It stays safe on this PC until you decide.
      </EmptyState>
    )
  }
  return (
    <>
      <VirtualList
        items={items}
        rowHeight={HELD_ROW_HEIGHT}
        getKey={(t) => t.id}
        labelledBy={labelledBy}
        label={labelledBy ? undefined : 'Held files'}
        renderRow={(t) => <HeldRow t={t} onDiscard={() => setDiscarding(t)} />}
      />
      {discarding && <DiscardDialog t={discarding} onClose={() => setDiscarding(null)} />}
    </>
  )
}

function HeldRow({ t, onDiscard }: { t: Transfer; onDiscard: () => void }) {
  const { client, shell } = useAgentContext()
  const toast = useToast()
  const { pending, error, run } = useAction()
  const held = t.held
  const actions = held?.actions ?? []
  const saveCopy = () =>
    run('save', async () => {
      const dir = await shell.pickFolder('Save a copy of ' + t.name)
      if (!dir) return
      const result = await client.saveHeldCopy(t.id, dir)
      toast({ icon: 'check', title: 'Saved a copy of ' + t.name, body: result.path, action: { label: 'Show', run: () => void client.open({ target: 'folder', path: dir }) } })
    })
  return (
    <article className="held-row" aria-label={t.name + ': ' + (held ? heldReasonTitle(held.code) : 'held')}>
      <span className="trow-icon state-held" aria-hidden="true">
        <Icon name="hold" size={16} />
      </span>
      <div className="held-main">
        <div className="held-head">
          <FileName name={t.name} className="trow-name" />
          <span className="held-tag">{held ? heldReasonTitle(held.code) : 'Held'}</span>
        </div>
        <p className="held-reason">{held?.message ?? 'The drive refused this file.'}</p>
        <p className="held-meta">
          <Truncate text={'Was going to ' + driveFolderOf(t.drive_path)} />
          <span className="held-meta-sep" aria-hidden="true" />
          <span>{formatBytes(t.size)}</span>
          {held?.since && (
            <>
              <span className="held-meta-sep" aria-hidden="true" />
              <span>{formatWhen(held.since)}</span>
            </>
          )}
        </p>
        {error && <p className="trow-error" role="alert">{error.message}</p>}
      </div>
      <div className="held-actions">
        {actions.includes('save_copy') && (
          <Button size="s" variant="secondary" icon="copy" pending={pending === 'save'} onClick={() => void saveCopy()}>
            Save a copy…
          </Button>
        )}
        {actions.includes('discard') && (
          <Button size="s" variant="danger-ghost" icon="trash" onClick={onDiscard}>
            Discard
          </Button>
        )}
      </div>
    </article>
  )
}

function DiscardDialog({ t, onClose }: { t: Transfer; onClose: () => void }) {
  const { client } = useAgentContext()
  const toast = useToast()
  const { pending, error, run } = useAction()
  return (
    <Dialog
      title={'Discard ' + t.name + '?'}
      tone="danger"
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose} data-autofocus>
            Keep it
          </Button>
          <Button
            variant="danger"
            pending={pending === 'discard'}
            onClick={() =>
              void run('discard', async () => {
                await client.discardHeld(t.id)
                toast({ icon: 'trash', title: 'Discarded ' + t.name })
                onClose()
              })
            }
          >
            Discard
          </Button>
        </>
      }
    >
      <p>The copy on this PC is deleted. It never reached the drive, so this can't be undone.</p>
      {error && <p className="field-error" role="alert">{error.message}</p>}
    </Dialog>
  )
}
