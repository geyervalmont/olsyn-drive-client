import { useCallback, useEffect, useId, useState, type ReactNode } from 'react'
import type { CacheState, CacheUsage, SettingsPatch, SettingsResponse } from '../../api/types'
import { AgentError } from '../../api/errors'
import { formatBytes, formatLimit } from '../../lib/format'
import { errorText, useAgentContext, useAgentView } from '../../state/context'
import { Truncate } from '../../ui/FileName'
import { Spinner } from '../../ui/Progress'
import { Button, Field, InlineError, Switch } from '../../ui/controls'

const GB = 1024 * 1024 * 1024
const MB = 1024 * 1024
const cacheSizes = [1, 2, 5, 10, 25, 50, 100, 200, 500].map((g) => g * GB)

/** The one-line answer to "is the drive filling up this PC?" (also shown on first run). */
export const cacheExplainer = 'Files live on the Olsyn drive. This PC keeps only what you opened recently and what is still uploading.'
const limits = [0, 1, 2, 5, 10, 25, 50, 100].map((m) => m * MB)

const fieldOf = (err: unknown): string | null => (err instanceof AgentError && typeof err.detail.field === 'string' ? err.detail.field : null)

export function Settings() {
  const { client, shell } = useAgentContext()
  const view = useAgentView()
  const [data, setData] = useState<SettingsResponse | null>(null)
  const [cache, setCache] = useState<CacheUsage | null | 'unsupported'>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [saving, setSaving] = useState<string | null>(null)
  const [errors, setErrors] = useState<Record<string, string>>({})
  const [freeing, setFreeing] = useState(false)
  const [freed, setFreed] = useState<string | null>(null)
  const titleId = useId()

  const load = useCallback(() => {
    setLoadError(null)
    client
      .settings()
      .then(setData)
      .catch((e: unknown) => setLoadError(errorText(e)))
    client
      .cache()
      .then((c) => setCache(c ?? 'unsupported'))
      .catch(() => setCache('unsupported'))
  }, [client])
  useEffect(load, [load, view.generation])

  // The state carries the live split; GET /v1/cache adds the folder and free disk.
  const live = view.state?.cache
  const usage: CacheUsage | null | 'unsupported' = cache && cache !== 'unsupported' && live ? { ...cache, ...live } : cache

  const freeUp = async () => {
    setFreeing(true)
    setFreed(null)
    try {
      const r = await client.freeCache()
      if (r) {
        setCache(r.cache)
        setFreed(r.freed_bytes > 0 ? 'Cleared ' + formatBytes(r.freed_bytes) + '. Those files are still on the drive.' : 'Nothing to clear: what is left is uploading or open.')
      }
    } catch (err) {
      setFreed(errorText(err))
    } finally {
      setFreeing(false)
    }
  }

  // A settings event (from this window or another) is the truth.
  useEffect(() => {
    if (view.settings) setData((d) => (d ? { ...d, settings: view.settings! } : d))
  }, [view.settings])

  const patch = async (key: string, p: SettingsPatch) => {
    if (!data) return
    const before = data
    setSaving(key)
    setErrors((e) => ({ ...e, [key]: '' }))
    // Optimistic: show the new value at once, roll back if the agent refuses.
    setData({ ...data, settings: mergePatch(data.settings, p) })
    try {
      setData(await client.patchSettings(p))
    } catch (err) {
      setData(before)
      const message = err instanceof AgentError && err.code === 'busy' ? 'Close the files open on the drive, then try again.' : errorText(err)
      setErrors((e) => ({ ...e, [fieldOf(err) ?? key]: message, [key]: message }))
    } finally {
      setSaving(null)
    }
  }

  if (loadError) return <InlineError message={loadError} onRetry={load} />
  if (!data) {
    return (
      <p className="list-foot">
        <Spinner size={14} /> Loading settings
      </p>
    )
  }
  const s = data.settings
  // the configured letter stays listed even when something else holds it now
  const letters = data.choices.drive_letters.includes(data.settings.drive_letter) ? data.choices.drive_letters : [data.settings.drive_letter, ...data.choices.drive_letters]
  const pending = data.pending_restart

  return (
    <section className="screen" aria-labelledby={titleId}>
      <header className="screen-head">
        <h1 id={titleId} className="screen-title display">
          Settings
        </h1>
      </header>

      <div className="settings">
        {letters.length > 0 && (
          <SettingsGroup title="Drive letter">
            <Field label="Letter" htmlFor="set-letter" hint="Changing it reconnects the drive. Paths in your files keep working only if every PC uses the same letter." error={errors.drive_letter}>
              <div className="field-row">
                <select id="set-letter" className="select" value={s.drive_letter} disabled={saving === 'drive_letter'} onChange={(e) => void patch('drive_letter', { drive_letter: e.target.value })}>
                  {letters.map((l) => (
                    <option key={l} value={l}>
                      {l}
                    </option>
                  ))}
                </select>
                {saving === 'drive_letter' && <Spinner size={14} label="Saving" />}
              </div>
            </Field>
          </SettingsGroup>
        )}

        <SettingsGroup title="Files kept on this PC">
          <p className="field-hint">{cacheExplainer}</p>
          <CacheBar usage={usage} cap={s.cache_cap_bytes} />
          {usage && usage !== 'unsupported' && usage.uploaded_bytes !== undefined && (
            <div className="field-row">
              <Button size="s" variant="secondary" pending={freeing} onClick={() => void freeUp()}>
                Free up space
              </Button>
              {freed && (
                <span className="field-hint" role="status">
                  {freed}
                </span>
              )}
            </div>
          )}
          <Field label="Most space to use" htmlFor="set-cache" hint="Above this, the oldest opened files are cleared first. Files still uploading are never cleared." error={errors.cache_cap_bytes}>
            <select id="set-cache" className="select" value={s.cache_cap_bytes} onChange={(e) => void patch('cache_cap_bytes', { cache_cap_bytes: Number(e.target.value) })}>
              {Array.from(new Set([...cacheSizes, s.cache_cap_bytes]))
                .sort((a, b) => a - b)
                .map((v) => (
                  <option key={v} value={v}>
                    {formatBytes(v)}
                  </option>
                ))}
            </select>
          </Field>
          <Field label="Where" htmlFor="set-cache-dir" error={errors.cache_dir} hint={pending.includes('cache_dir') ? 'Moves the next time Olsyn Drive starts.' : undefined}>
            <div className="field-row">
              <span id="set-cache-dir" className="path-box">
                <Truncate text={s.cache_dir} />
              </span>
              <Button
                size="s"
                variant="secondary"
                pending={saving === 'cache_dir'}
                onClick={() =>
                  void shell.pickFolder('Choose where Olsyn Drive keeps files').then((dir) => {
                    if (dir) void patch('cache_dir', { cache_dir: dir })
                  })
                }
              >
                Change…
              </Button>
            </div>
          </Field>
        </SettingsGroup>

        <SettingsGroup title="Speed">
          <div className="field-pair">
            <LimitField id="set-up" label="Upload limit" value={s.bandwidth.up_bps} error={errors.bandwidth} onChange={(v) => void patch('bandwidth', { bandwidth: { up_bps: v } })} />
            <LimitField id="set-down" label="Download limit" value={s.bandwidth.down_bps} onChange={(v) => void patch('bandwidth', { bandwidth: { down_bps: v } })} />
          </div>
          <p className="field-hint">Limit uploads if video calls stutter while files go up. Takes effect at once.</p>
        </SettingsGroup>

        <SettingsGroup title="Starting and updates">
          <Switch checked={s.start_at_login} onChange={(v) => void patch('start_at_login', { start_at_login: v })} label="Start when I sign in to Windows" description="Keeps the drive letter there from the moment you sign in." />
          {errors.start_at_login && <p className="field-error">{errors.start_at_login}</p>}
          <Field label="Updates" htmlFor="set-channel" error={errors.update_channel} hint={s.update_channel === 'beta' ? 'Beta gets new versions a week or two early. They may have rough edges.' : 'Stable versions, tested on beta first.'}>
            <select id="set-channel" className="select" value={s.update_channel} onChange={(e) => void patch('update_channel', { update_channel: e.target.value })}>
              {data.choices.update_channels.map((c) => (
                <option key={c} value={c}>
                  {c === 'stable' ? 'Stable' : c === 'beta' ? 'Beta' : c}
                </option>
              ))}
            </select>
          </Field>
        </SettingsGroup>

        <SettingsGroup title="Notifications">
          <Switch checked={s.notifications.finished} onChange={(v) => void patch('notifications', { notifications: { finished: v } })} label="When a copy has finished" />
          <Switch checked={s.notifications.held} onChange={(v) => void patch('notifications', { notifications: { held: v } })} label="When the drive refuses a file" description="Also when a second copy is saved because a file changed on the drive." />
          <Switch checked={s.notifications.signed_out} onChange={(v) => void patch('notifications', { notifications: { signed_out: v } })} label="When this PC is signed out" />
          <Switch checked={s.notifications.update_applied} onChange={(v) => void patch('notifications', { notifications: { update_applied: v } })} label="When an update is installed" />
          {errors.notifications && <p className="field-error">{errors.notifications}</p>}
        </SettingsGroup>
      </div>
    </section>
  )
}

function SettingsGroup({ title, children }: { title: string; children: ReactNode }) {
  const id = useId()
  return (
    <section className="settings-group" aria-labelledby={id}>
      <h2 id={id} className="settings-title">
        {title}
      </h2>
      <div className="settings-body">{children}</div>
    </section>
  )
}

function LimitField({ id, label, value, onChange, error }: { id: string; label: string; value: number; onChange: (v: number) => void; error?: string }) {
  const options = Array.from(new Set([...limits, value])).sort((a, b) => (a === 0 ? -1 : b === 0 ? 1 : a - b))
  return (
    <Field label={label} htmlFor={id} error={error}>
      <select id={id} className="select" value={value} onChange={(e) => onChange(Number(e.target.value))}>
        {options.map((v) => (
          <option key={v} value={v}>
            {formatLimit(v)}
          </option>
        ))}
      </select>
    </Field>
  )
}

/** The cache's space in three plain parts; older agents (no split) show used and unsent only. */
function CacheBar({ usage, cap }: { usage: (CacheUsage & Partial<CacheState>) | null | 'unsupported'; cap: number }) {
  if (usage === 'unsupported') return null
  if (!usage) return <div className="cache-bar is-loading" aria-hidden="true" />
  if (usage.uploaded_bytes === undefined || usage.opened_bytes === undefined) return <OldCacheBar usage={usage} cap={cap} />
  const scale = Math.max(cap, usage.used_bytes)
  const parts = [
    { key: 'unsent', label: 'Waiting to upload', bytes: usage.unsent_bytes },
    { key: 'uploaded', label: 'Just uploaded, clearing soon', bytes: usage.uploaded_bytes },
    { key: 'opened', label: 'Recently opened', bytes: usage.opened_bytes },
  ]
  return (
    <div className="cache">
      <div
        className="cache-bar is-split"
        role="meter"
        aria-label="Space used on this PC"
        aria-valuemin={0}
        aria-valuemax={cap}
        aria-valuenow={usage.used_bytes}
        aria-valuetext={formatBytes(usage.used_bytes) + ' of ' + formatBytes(cap)}
      >
        {parts.map((p) => (p.bytes > 0 ? <span key={p.key} className={'cache-part cache-' + p.key} style={{ width: (p.bytes / scale) * 100 + '%' }} /> : null))}
      </div>
      <p className="cache-legend tnum">
        {parts.map((p) => (
          <span key={p.key}>
            <span className={'swatch swatch-' + p.key} aria-hidden="true" />
            {p.label} {formatBytes(p.bytes)}
          </span>
        ))}
      </p>
      <p className="cache-legend tnum">
        <span>
          {formatBytes(usage.used_bytes)} of {formatBytes(cap)} on this PC
        </span>
        <span className="muted">{formatBytes(usage.disk_free_bytes)} free on the disk</span>
      </p>
    </div>
  )
}

function OldCacheBar({ usage, cap }: { usage: CacheUsage; cap: number }) {
  const used = Math.min(1, usage.used_bytes / cap)
  const unsent = Math.min(used, usage.unsent_bytes / cap)
  return (
    <div className="cache">
      <div
        className="cache-bar"
        role="meter"
        aria-label="Space used on this PC"
        aria-valuemin={0}
        aria-valuemax={cap}
        aria-valuenow={usage.used_bytes}
        aria-valuetext={formatBytes(usage.used_bytes) + ' of ' + formatBytes(cap)}
      >
        <span className="cache-used" style={{ width: used * 100 + '%' }} />
        <span className="cache-unsent" style={{ width: unsent * 100 + '%' }} />
      </div>
      <p className="cache-legend tnum">
        <span>
          <span className="swatch swatch-used" aria-hidden="true" />
          {formatBytes(usage.used_bytes)} of {formatBytes(cap)} used
        </span>
        {usage.unsent_bytes > 0 && (
          <span>
            <span className="swatch swatch-unsent" aria-hidden="true" />
            {formatBytes(usage.unsent_bytes)} not sent yet
          </span>
        )}
        <span className="muted">{formatBytes(usage.disk_free_bytes)} free on the disk</span>
      </p>
    </div>
  )
}

function mergePatch<T>(target: T, patch: unknown): T {
  if (!patch || typeof patch !== 'object') return patch as T
  const out: Record<string, unknown> = { ...(target as Record<string, unknown>) }
  for (const [k, v] of Object.entries(patch as Record<string, unknown>)) {
    if (v === null) delete out[k]
    else if (typeof v === 'object' && !Array.isArray(v)) out[k] = mergePatch(out[k] ?? {}, v)
    else out[k] = v
  }
  return out as T
}
