// Banners for conditions that change what the person can do: offline, the
// drive restarting, signed out by the drive, and anything needing attention.
// Each says what happened and offers the one action that helps.

import type { Issue } from '../api/types'
import { useAction, useAgentContext, useAgentView } from '../state/context'
import { Icon, type IconName } from '../ui/Icon'
import { Button } from '../ui/controls'

type BannerTone = 'info' | 'warn' | 'danger' | 'quiet'

interface BannerModel {
  key: string
  tone: BannerTone
  icon: IconName
  title: string
  body?: string
  action?: { label: string; run: () => Promise<unknown> | void }
  /** A second, quieter choice (e.g. "Leave off"). */
  secondary?: { label: string; run: () => Promise<unknown> | void }
}

export function useBanners(navigate: (route: string) => void): BannerModel[] {
  const view = useAgentView()
  const { client, connection, shell } = useAgentContext()
  const state = view.state
  const banners: BannerModel[] = []

  if (view.blocked) {
    const blocked = view.blocked
    banners.push({
      key: 'blocked',
      tone: 'danger',
      icon: 'shield',
      title: blocked.title,
      body: blocked.detail,
      action: { label: 'What to do', run: () => shell.openUrl(blocked.helpUrl) },
      secondary: { label: 'Try again', run: () => connection.retryNow() },
    })
  } else if (view.stream === 'unreachable') {
    banners.push({
      key: 'unreachable',
      tone: 'danger',
      icon: 'plug',
      title: "Olsyn Drive isn't running on this PC.",
      body: 'Starting it again. If this stays, restart the PC.',
      action: { label: 'Try again', run: () => connection.retryNow() },
    })
  } else if (view.stream === 'reconnecting' && state) {
    banners.push({ key: 'reconnecting', tone: 'quiet', icon: 'refresh', title: 'Reconnecting to Olsyn Drive.', body: 'What you see may be a few seconds old.' })
  }
  if (!state) return banners

  if (state.connection === 'offline') {
    banners.push({
      key: 'offline',
      tone: 'info',
      icon: 'offline',
      title: "You're offline.",
      body: 'Files you save wait on this PC and upload when the drive is back.',
    })
  }
  if (state.connection === 'server_restarting') {
    banners.push({
      key: 'restarting',
      tone: 'info',
      icon: 'refresh',
      title: 'The drive is restarting.',
      body: 'Uploads are waiting and will carry on by themselves. Nothing is lost.',
    })
  }
  if (!state.account && state.reason?.code === 'token_revoked') {
    banners.push({
      key: 'revoked',
      tone: 'warn',
      icon: 'key',
      title: 'Signed out by the drive.',
      body: 'This PC was removed from your account in the web app. Sign in again to carry on.',
      action: { label: 'Sign in', run: () => navigate('signin') },
    })
  }
  for (const issue of state.issues) banners.push(issueBanner(issue, client, navigate))
  if (state.paused && state.connection !== 'offline') {
    const until = state.paused_until
      ? 'Uploads start again at ' + new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(new Date(state.paused_until)) + '.'
      : 'Nothing uploads until you resume.'
    banners.push({ key: 'paused', tone: 'quiet', icon: 'pause', title: 'Uploads are paused.', body: until, action: { label: 'Resume', run: () => client.resume() } })
  }
  if (state.update.state === 'ready' && !state.issues.some((i) => i.code === 'update_required')) {
    banners.push({
      key: 'update-ready',
      tone: 'quiet',
      icon: 'sparkle',
      title: 'Version ' + (state.update.available ?? '') + ' is ready.',
      body: 'It installs by itself when nothing is moving, or now if you restart.',
      action: { label: 'Restart now', run: () => client.applyUpdate('now') },
    })
  }
  return banners
}

function issueBanner(issue: Issue, client: ReturnType<typeof useAgentContext>['client'], navigate: (route: string) => void): BannerModel {
  const base = { key: 'issue-' + issue.code, tone: 'warn' as const, icon: 'alert' as IconName, title: issue.message }
  switch (issue.action) {
    case 'show_held':
      return { ...base, icon: 'hold', body: 'They are kept on this PC, not on the drive. Choose what to do with each one.', action: { label: 'Show files', run: () => navigate('transfers/held') } }
    case 'install_driver':
      return { ...base, icon: 'shield', body: 'Windows will ask for permission once.', action: { label: 'Install', run: () => client.installDriver() } }
    case 'enable_autostart':
      return {
        ...base,
        icon: 'gear',
        body: 'Turn it back on so the drive is there when you sign in. Nothing changes until you choose.',
        action: { label: 'Turn back on', run: () => client.patchSettings({ start_at_login: true }) },
        secondary: { label: 'Leave off', run: () => client.patchSettings({ start_at_login: false }) },
      }
    case 'check_driver':
      return { ...base, icon: 'shield', action: { label: 'Check again', run: () => client.checkDriver() } }
    case 'mount':
      return { ...base, icon: 'drive', action: { label: 'Connect again', run: () => client.mount() } }
    case 'apply_update':
      return { ...base, tone: 'danger', icon: 'sparkle', action: { label: 'Install update', run: () => client.applyUpdate('now') } }
    case 'check_update':
      return { ...base, icon: 'sparkle', action: { label: 'Check again', run: () => client.checkUpdate() } }
    case 'show_settings':
      return { ...base, icon: 'gear', action: { label: 'Open settings', run: () => navigate('settings') } }
    case 'sign_in':
      return { ...base, icon: 'key', action: { label: 'Sign in', run: () => navigate('signin') } }
    default:
      return base
  }
}

export function Banners({ navigate, compact = false }: { navigate: (route: string) => void; compact?: boolean }) {
  const banners = useBanners(navigate)
  if (banners.length === 0) return null
  const shown = compact ? banners.slice(0, 1) : banners
  return (
    <div className={'banners' + (compact ? ' is-compact' : '')}>
      {shown.map((b) => (
        <Banner key={b.key} model={b} compact={compact} />
      ))}
    </div>
  )
}

function Banner({ model, compact }: { model: BannerModel; compact: boolean }) {
  const { pending, error, run } = useAction()
  const urgent = model.tone === 'warn' || model.tone === 'danger'
  return (
    <div className={'banner banner-' + model.tone} role={urgent ? 'alert' : 'status'}>
      <span className="banner-icon">
        <Icon name={model.icon} size={18} />
      </span>
      <div className="banner-text">
        <p className="banner-title">{model.title}</p>
        {model.body && !compact && <p className="banner-body">{model.body}</p>}
        {error && <p className="banner-error">{error.message}</p>}
      </div>
      {model.secondary && (
        <Button size="s" variant="ghost" pending={pending === 'b'} onClick={() => void run('b', async () => model.secondary!.run())}>
          {model.secondary.label}
        </Button>
      )}
      {model.action && (
        <Button size="s" variant={urgent ? 'primary' : 'secondary'} pending={pending === 'a'} onClick={() => void run('a', async () => model.action!.run())}>
          {model.action.label}
        </Button>
      )}
    </div>
  )
}
