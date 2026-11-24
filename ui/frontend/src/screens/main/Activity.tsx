import { useEffect, useId, useMemo, useState } from 'react'
import type { AgentNotification } from '../../api/types'
import { formatWhen } from '../../lib/format'
import { useNow } from '../../lib/useNow'
import { errorText, useAgentContext, useAgentView } from '../../state/context'
import { Icon, type IconName } from '../../ui/Icon'
import { Spinner } from '../../ui/Progress'
import { EmptyState, InlineError } from '../../ui/controls'

const kindIcon: Record<string, IconName> = {
  finished: 'check',
  held: 'hold',
  conflict: 'copy',
  signed_out: 'key',
  signed_in: 'user',
  mounted: 'drive',
  update_applied: 'sparkle',
  update_failed: 'alert',
  error: 'alert',
}

export function Activity({ navigate }: { navigate: (route: string) => void }) {
  const view = useAgentView()
  const { client, shell } = useAgentContext()
  const [history, setHistory] = useState<AgentNotification[] | null>(null)
  const [error, setError] = useState<string | null>(null)
  const now = useNow(30_000)
  const titleId = useId()

  const load = () => {
    setError(null)
    client
      .activity()
      .then((page) => setHistory(page?.items ?? []))
      .catch((e: unknown) => setError(errorText(e)))
  }
  useEffect(load, [client, view.generation])

  // History from the agent (GET /v1/activity) plus what arrived since.
  const items = useMemo(() => {
    const seen = new Set<string>()
    const out: AgentNotification[] = []
    for (const n of [...view.notifications, ...(history ?? [])]) {
      if (seen.has(n.id)) continue
      seen.add(n.id)
      out.push(n)
    }
    return out.sort((a, b) => Date.parse(b.at) - Date.parse(a.at))
  }, [view.notifications, history])

  const act = (n: AgentNotification) => {
    const open = n.action?.open
    if (!open) return
    if (open.target === 'ui') navigate(open.path ?? 'transfers')
    else void client.open(open).catch(() => shell.openUrl(view.state?.account?.server ?? 'https://drive.olsyn.com'))
  }

  return (
    <section className="screen" aria-labelledby={titleId}>
      <header className="screen-head">
        <h1 id={titleId} className="screen-title display">
          Activity
        </h1>
      </header>
      {error && <InlineError message={error} onRetry={load} />}
      {!history && !error && (
        <p className="list-foot">
          <Spinner size={14} /> Loading activity
        </p>
      )}
      {history && items.length === 0 && (
        <EmptyState icon="activity" title="Nothing has happened yet.">
          Sign-ins, finished copies, held files and updates are listed here as they happen.
        </EmptyState>
      )}
      {items.length > 0 && (
        <ol className="activity" aria-label="Recent activity">
          {items.map((n) => (
            <li key={n.id} className={'activity-item kind-' + n.kind}>
              <span className="activity-icon" aria-hidden="true">
                <Icon name={kindIcon[n.kind] ?? 'info'} size={16} />
              </span>
              <div className="activity-text">
                <p className="activity-title">{n.title}</p>
                {n.body && <p className="activity-body">{n.body}</p>}
              </div>
              <time className="activity-when" dateTime={n.at} title={new Date(n.at).toLocaleString()}>
                {formatWhen(n.at, now)}
              </time>
              {n.action && (
                <button type="button" className="link-btn activity-action" onClick={() => act(n)}>
                  {n.action.label}
                </button>
              )}
            </li>
          ))}
        </ol>
      )}
    </section>
  )
}
