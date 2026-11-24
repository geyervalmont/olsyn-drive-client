// Send a folder (or files) from this PC to a folder on the drive. Opened by the
// "Send a folder…" button or by dropping onto the window; asks where it goes.

import { useCallback, useEffect, useState } from 'react'
import type { DriveFolderList } from '../../api/types'
import { baseName, formatBytes, plural } from '../../lib/format'
import { errorText, useAction, useAgentContext } from '../../state/context'
import { Dialog } from '../../ui/Dialog'
import { Truncate } from '../../ui/FileName'
import { Icon } from '../../ui/Icon'
import { Spinner } from '../../ui/Progress'
import { useToast } from '../../ui/Toasts'
import { Button, Field, InlineError } from '../../ui/controls'

export function SendDialog({ initialPaths, onClose, onSent }: { initialPaths: string[]; onClose: () => void; onSent: () => void }) {
  const { client, shell } = useAgentContext()
  const toast = useToast()
  const [paths, setPaths] = useState(initialPaths)
  const [dest, setDest] = useState('/')
  const { pending, error, run } = useAction()

  const choose = () =>
    run('choose', async () => {
      const p = await shell.pickFolder('Choose a folder to send')
      if (p) setPaths([p])
    })

  const send = () =>
    run('send', async () => {
      const r = await client.send(paths, dest)
      const { files, bytes } = r
      const what = paths.length === 1 ? baseName(paths[0]) : plural(paths.length, 'item')
      toast({ icon: 'send', title: 'Sending ' + what, body: plural(files, 'file') + ', ' + formatBytes(bytes) + ', to ' + (dest === '/' ? 'the top of the drive' : dest) + '.' })
      onSent()
    })

  const destName = dest === '/' ? 'the top of the drive' : baseName(dest)
  return (
    <Dialog
      title="Send to the drive"
      wide
      onClose={onClose}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" icon="send" disabled={paths.length === 0} pending={pending === 'send'} onClick={() => void send()}>
            {'Send to ' + destName}
          </Button>
        </>
      }
    >
      <div className="send-what">
        <p className="send-label">What to send</p>
        {paths.length === 0 ? (
          <Button variant="secondary" icon="folder" pending={pending === 'choose'} onClick={() => void choose()} data-autofocus>
            Choose a folder…
          </Button>
        ) : (
          <ul className="send-paths">
            {paths.map((p) => (
              <li key={p}>
                <Icon name="folder" size={16} />
                <Truncate text={p} />
              </li>
            ))}
          </ul>
        )}
      </div>
      <FolderBrowser value={dest} onChange={setDest} />
      {error && <InlineError message={error.message} />}
    </Dialog>
  )
}

/** Browses drive folders through GET /v1/drive/folders; falls back to typing a path with an older agent. */
function FolderBrowser({ value, onChange }: { value: string; onChange: (path: string) => void }) {
  const { client } = useAgentContext()
  const [list, setList] = useState<DriveFolderList | null | 'unsupported'>(null)
  const [err, setErr] = useState<string | null>(null)
  const load = useCallback(
    (path: string) => {
      setErr(null)
      client
        .driveFolders(path)
        .then((r) => setList(r ?? 'unsupported'))
        .catch((e: unknown) => setErr(errorText(e)))
    },
    [client],
  )
  useEffect(() => load(value), [load, value])

  if (list === 'unsupported') {
    return (
      <Field label="Where on the drive" htmlFor="send-dest" hint="A folder path on the drive, like /Projects/Harbour Tower.">
        <input id="send-dest" className="input" value={value} onChange={(e) => onChange(e.target.value || '/')} />
      </Field>
    )
  }
  const parts = value.split('/').filter(Boolean)
  return (
    <div className="send-where">
      <p className="send-label" id="send-where-label">
        Where on the drive
      </p>
      <nav className="crumbs" aria-label="Folder path">
        <button type="button" className="crumb" onClick={() => onChange('/')} aria-current={parts.length === 0 ? 'location' : undefined}>
          Drive
        </button>
        {parts.map((p, i) => (
          <span key={i} className="crumb-wrap">
            <Icon name="chevronRight" size={14} />
            <button type="button" className="crumb" onClick={() => onChange('/' + parts.slice(0, i + 1).join('/'))} aria-current={i === parts.length - 1 ? 'location' : undefined}>
              {p}
            </button>
          </span>
        ))}
      </nav>
      <div className="folder-list" role="list" aria-labelledby="send-where-label">
        {err && <InlineError message={err} onRetry={() => load(value)} />}
        {!list && !err && (
          <p className="list-foot">
            <Spinner size={14} /> Loading folders
          </p>
        )}
        {list && typeof list === 'object' && list.folders.length === 0 && <p className="list-foot">No folders inside. Files go straight into this one.</p>}
        {list &&
          typeof list === 'object' &&
          list.folders.map((f) => (
            <div role="listitem" key={f.path}>
              <button type="button" className="folder-item" onClick={() => onChange(f.path)} disabled={!f.can_write} title={f.can_write ? f.name : f.name + ': you can view this folder but not add to it'}>
                <Icon name="folder" size={17} />
                <span className="truncate">{f.name}</span>
                {!f.can_write ? <span className="folder-ro">View only</span> : <Icon name="chevronRight" size={15} />}
              </button>
            </div>
          ))}
      </div>
    </div>
  )
}
