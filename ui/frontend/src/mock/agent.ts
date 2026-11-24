// An in-process stand-in for the agent: the same endpoints, the same events and
// the same shapes as docs/ipc.md, driven by a simulated clock. Used by
// `npm run dev` (?scenario=...) and by tests (tick() by hand).

import type { SSEMessage } from '../api/sse'
import type {
  Account,
  AgentNotification,
  CodeMessage,
  Driver,
  Issue,
  LetterUse,
  ListState,
  Mount,
  Settings,
  SettingsResponse,
  SignIn,
  State,
  Summary,
  Transfer,
  TransferCounts,
  Update,
} from '../api/types'
import { bigFile, driveTree, localPath, readOnlyFolders, seededRandom, smallFiles, winPath, type FileSpec } from './data'
import type { ScenarioName } from './scenarios'

export interface MockResponse {
  status: number
  body: unknown
}

export interface MockSubscriber {
  onOpen(): void
  onMessage(message: SSEMessage): void
  onError(error: Error): void
}

export interface MockAgentOptions {
  scenario: ScenarioName
  /** Simulated seconds per real second (dev: ?speed=). */
  speed?: number
  /** Real ms between ticks when started with start(). */
  tickMs?: number
  seed?: number
  /** Wall clock used for timestamps. */
  now?: () => number
}

const MB = 1024 * 1024
const GB = 1024 * MB
const PART = 16 * MB
const SLOTS = 4
/** A small file still costs a pre-check and a complete: about this long, in seconds. */
const PER_FILE_OVERHEAD = 0.18
const HISTORY_LIMIT = 1000
const LOG_LIMIT = 1024

type Internal = Transfer & { skipAs: FileSpec['skip']; overhead: number }

const err = (status: number, error: string, message: string, extra: Record<string, unknown> = {}): MockResponse => ({
  status,
  body: { error, message, ...extra },
})
const ok = (body: unknown, status = 200): MockResponse => ({ status, body })

/** The configured letter if free, else the next free one towards Z:, then the nearest before it (as the agent does). */
export function suggestLetter(want: string, inUse: Record<string, string>): string {
  const code = /^[D-Z]:$/.test(want) ? want.charCodeAt(0) : 87
  const free = (c: number) => !inUse[String.fromCharCode(c) + ':']
  if (free(code)) return String.fromCharCode(code) + ':'
  for (let c = code + 1; c <= 90; c++) if (free(c)) return String.fromCharCode(c) + ':'
  for (let c = code - 1; c >= 68; c--) if (free(c)) return String.fromCharCode(c) + ':'
  return ''
}

export class MockAgent {
  readonly scenario: ScenarioName
  private readonly speed: number
  private readonly tickMs: number
  private readonly random: () => number
  private readonly wall: () => number
  private timer: ReturnType<typeof setInterval> | null = null

  private sim = 0 // simulated seconds since start
  private run = 'r' + Math.random().toString(36).slice(2, 8)
  private seq = 0
  private log: SSEMessage[] = []
  private subscribers = new Set<MockSubscriber>()
  private scheduled: { at: number; fn: () => void }[] = []

  private version = '1.4.2'
  private startedAt: string
  private account: Account | null = null
  private reason: CodeMessage | null = null
  private signin: SignIn | null = null
  private startingUntil = 0
  private online = true
  private restartingUntil = 0
  private paused = false
  private pausedUntil: number | null = null
  private mount: Mount = { state: 'unmounted', letter: 'W:', path: null, error: null }
  private driver: Driver = { state: 'ok', name: 'winfsp', version: '2.2.25089', min_version: '2.0', error: null }
  private update: Update
  private settings: Settings
  private transfers = new Map<string, Internal>()
  private queue: string[] = []
  private held: string[] = []
  private history: string[] = []
  private nextId = 1
  private workTotal = 0
  private workDone = 0
  private rate = 0
  private activity: AgentNotification[] = []
  private notificationSeq = 1
  private lastStateJson = ''
  private lastSummaryJson = ''
  private agentDown = false
  /** Failures left before the agent "starts" (unreachable scenario). */
  private downAttempts = 0
  private snapshotDelay = 0
  // the cache's split (bytes): just uploaded, and opened files
  private cacheUploaded = Math.round(2.1 * GB)
  private cacheOpened = Math.round(1.3 * GB)

  constructor(options: MockAgentOptions) {
    this.scenario = options.scenario
    this.speed = options.speed ?? 1
    this.tickMs = options.tickMs ?? 250
    this.random = seededRandom(options.seed ?? 7)
    this.wall = options.now ?? (() => Date.now())
    this.startedAt = this.iso(0)
    this.settings = {
      drive_letter: 'W:',
      cache_dir: localPath('C:', 'Users', 'ada', 'AppData', 'Local', 'Olsyn Drive', 'cache'),
      cache_cap_bytes: 10 * GB,
      cache_release: { after_upload_seconds: 300, keep_opened_seconds: 1800 },
      bandwidth: { up_bps: 0, down_bps: 0 },
      start_at_login: true,
      update_channel: 'stable',
      notifications: { finished: true, held: true, signed_out: true, update_applied: true },
      server_url: 'https://drive.olsyn.com',
      device_name: '',
    }
    this.update = {
      state: 'up_to_date', channel: 'stable', current: this.version, available: null, notes: null, published_at: null,
      required: false, driver_required: null, progress: null, error: null, checked_at: this.iso(-3600), apply: 'when_idle',
    }
    this.setup()
    this.lastStateJson = JSON.stringify(this.stateWithoutSummary())
  }

  // ---------- clock ----------

  start() {
    if (this.timer) return
    this.timer = setInterval(() => this.tick(this.tickMs / 1000), this.tickMs)
  }

  stop() {
    if (this.timer) clearInterval(this.timer)
    this.timer = null
  }

  /** Advance the simulation by real seconds (scaled by speed). */
  tick(realSeconds: number) {
    const dt = realSeconds * this.speed
    this.sim += dt
    const due = this.scheduled.filter((s) => s.at <= this.sim)
    this.scheduled = this.scheduled.filter((s) => s.at > this.sim)
    for (const s of due) s.fn()
    if (this.pausedUntil !== null && this.wallMs() >= this.pausedUntil) {
      this.paused = false
      this.pausedUntil = null
    }
    if (this.signin?.state === 'waiting' && Date.parse(this.signin.expires_at) <= this.wallMs()) {
      this.signin = { ...this.signin, state: 'expired', error: { code: 'link_expired', message: 'The code ran out before it was approved.' } }
      this.reason = { code: 'link_expired', message: 'The sign-in code ran out. Start again to get a new one.' }
      this.emit('signin', this.signin)
    }
    this.moveFiles(dt)
    this.publishState()
  }

  private at(delaySeconds: number, fn: () => void) {
    this.scheduled.push({ at: this.sim + delaySeconds, fn })
  }

  private wallMs() {
    return this.wall() + this.sim * 1000
  }

  private iso(offsetSeconds = 0) {
    return new Date(this.wallMs() + offsetSeconds * 1000).toISOString()
  }

  // ---------- scenarios ----------

  private signInAs() {
    this.account = {
      server: this.settings.server_url,
      user_id: 'u_6f1c2a',
      name: 'Ada Example',
      email: 'ada@example.com',
      device_id: 'd_0b7e41',
      read_only: false,
      linked_at: this.iso(-86400 * 12),
    }
    this.reason = null
  }

  /** The person has chosen the drive letter (first run: not yet). */
  private letterConfirmed = true
  private declinedOnce = false
  /** Letters something else holds on this "PC". */
  private lettersInUse: Record<string, LetterUse> = { 'D:': 'volume' }

  private mounted() {
    this.mount = { state: 'mounted', letter: this.settings.drive_letter, path: this.settings.drive_letter + String.fromCharCode(92), error: null }
  }

  private setup() {
    const s = this.scenario
    if (s === 'first-run' || s === 'signin' || s === 'link-expired' || s === 'letter-taken' || s === 'driver-restart' || s === 'driver-declined') {
      this.letterConfirmed = false
      if (s === 'first-run' || s === 'driver-restart' || s === 'driver-declined') this.driver = { state: 'missing', name: 'winfsp', version: null, min_version: '2.0', error: null }
      if (s === 'letter-taken') this.lettersInUse = { 'D:': 'volume', 'O:': 'network', 'W:': 'network' }
      return
    }
    if (s === 'signed-out') {
      this.reason = { code: 'token_revoked', message: 'This PC was removed from your account in the web app.' }
      this.addActivity('signed_out', 'Signed out by the drive', 'This PC was removed from your account in the web app.', null)
      this.at(1.5, () =>
        this.notify('signed_out', 'Signed out by the drive', 'Sign in again to keep using W: on this PC.', { label: 'Sign in', open: { target: 'ui', path: 'signin' } }),
      )
      return
    }
    if (s === 'unreachable') {
      this.downAttempts = 5
    }
    if (s === 'loading') this.snapshotDelay = 3
    this.signInAs()
    if (s === 'driver-missing') {
      this.driver = { state: 'missing', name: 'winfsp', version: null, min_version: '2.0', error: null }
      this.mount = { state: 'failed', letter: 'W:', path: null, error: { code: 'driver_missing', message: 'The file system driver is not installed.' } }
      return
    }
    this.mounted()
    this.seedHistory()
    if (s === 'empty') {
      this.history = []
      this.transfers.clear()
      this.activity = []
      return
    }
    if (s === 'copy' || s === 'restart' || s === 'offline' || s === 'loading' || s === 'unreachable') {
      this.queueCopy(3000, true)
    }
    if (s === 'restart') {
      this.at(14, () => {
        this.restartingUntil = this.sim + 16
        this.addActivity('error', 'The drive restarted', 'Uploads waited and carried on by themselves.', null)
      })
    }
    if (s === 'offline') this.online = false
    if (s === 'big-list') {
      this.paused = true
      this.queueCopy(12000, false)
    }
    if (s === 'held') this.seedHeld()
    if (s === 'conflict') this.seedConflict()
    if (s === 'update') {
      this.update = {
        ...this.update, state: 'available', available: '1.5.0', notes: 'Folders with thousands of files open faster. Clearer messages when a file is held.',
        published_at: this.iso(-86400 * 3), checked_at: this.iso(-600),
      }
    }
  }

  private makeTransfer(spec: FileSpec, origin: Transfer['origin'], state: Transfer['state'], batch: string | null = null): Internal {
    const id = 't_' + String(this.nextId++).padStart(6, '0')
    const t: Internal = {
      id,
      direction: 'up',
      origin,
      state,
      name: spec.name,
      local_path: origin === 'send' ? localPath('D:', 'Exports', ...spec.drivePath.split('/').filter(Boolean).slice(-2)) : winPath(this.settings.drive_letter, spec.drivePath),
      drive_path: spec.drivePath,
      node_id: null,
      size: spec.size,
      bytes_done: 0,
      bps: 0,
      eta_seconds: null,
      attempts: 0,
      queued_at: this.iso(),
      started_at: null,
      finished_at: null,
      error: null,
      held: null,
      skipped_reason: null,
      conflict_copy: null,
      batch_id: batch,
      can: { retry: false, cancel: origin === 'send' },
      skipAs: spec.skip,
      overhead: PER_FILE_OVERHEAD,
    }
    this.transfers.set(id, t)
    return t
  }

  private queueCopy(n: number, withBig: boolean) {
    const files = smallFiles(n, this.random)
    if (withBig) files.splice(3, 0, bigFile)
    for (const f of files) {
      const t = this.makeTransfer(f, 'mount', 'waiting')
      this.queue.push(t.id)
      this.workTotal += f.size
    }
  }

  private seedHistory() {
    const files = smallFiles(14, seededRandom(99))
    files.forEach((f, i) => {
      const t = this.makeTransfer(f, 'mount', 'done')
      t.bytes_done = t.size
      t.started_at = this.iso(-3600 * (i + 1) - 30)
      t.finished_at = this.iso(-3600 * (i + 1))
      if (f.skip) {
        t.state = 'skipped'
        t.skipped_reason = f.skip
      }
      this.history.push(t.id)
    })
    this.addActivity('finished', 'Sent 14 files', 'Harbour Tower site photos are on the drive.', null, -3600)
  }

  private seedHeld() {
    const specs: [FileSpec, Transfer['held']][] = [
      [
        { name: 'Tower model.ifc', drivePath: '/Projects/Harbour Tower/Models/Tower model.ifc', size: 940 * MB, skip: null },
        { code: 'checksum_mismatch', message: "The drive's copy of this file does not match what this PC sent. Re-upload it from this PC to replace the damaged copy.", since: this.iso(-1800), actions: ['reupload', 'save_copy', 'discard'] },
      ],
      [
        { name: 'Client issue set 07.pdf', drivePath: '/Projects/Harbour Tower/Client issue (managed)/Client issue set 07.pdf', size: 18 * MB, skip: null },
        { code: 'managed', message: 'Another app keeps this folder in step, so files cannot be added here by hand. Save a copy somewhere else, or ask whoever runs that app.', since: this.iso(-900), actions: ['save_copy', 'discard'] },
      ],
      [
        { name: 'Archive index.xlsx', drivePath: '/Projects/Archive (read only)/Archive index.xlsx', size: 420 * 1024, skip: null },
        { code: 'forbidden', message: 'You can open files in this folder but not add to it. Ask the folder owner for edit access, or save a copy somewhere else.', since: this.iso(-600), actions: ['save_copy', 'discard'] },
      ],
      [
        { name: 'Lobby option final.', drivePath: '/Projects/Harbour Tower/Renders/Lobby/Lobby option final.', size: 6.2 * MB, skip: null },
        { code: 'invalid_name', message: 'The drive cannot store a name that ends with a dot. Save a copy, rename it, then put it back.', since: this.iso(-120), actions: ['save_copy', 'discard'] },
      ],
    ]
    for (const [spec, held] of specs) {
      const t = this.makeTransfer(spec, 'mount', 'held')
      t.held = held
      t.attempts = 1
      t.bytes_done = 0
      this.held.unshift(t.id)
    }
    this.addActivity('held', "Couldn't save 4 files", 'The drive refused them. They are safe on this PC.', { label: 'Show', open: { target: 'ui', path: 'transfers/held' } })
  }

  private seedConflict() {
    const spec: FileSpec = { name: 'Level 3 plan.dwg', drivePath: '/Projects/Harbour Tower/Drawings/Working/Level 3 plan.dwg', size: 7.4 * MB, skip: null }
    const t = this.makeTransfer(spec, 'mount', 'done')
    t.bytes_done = t.size
    t.started_at = this.iso(-50)
    t.finished_at = this.iso(-40)
    t.conflict_copy = '/Projects/Harbour Tower/Drawings/Working/Level 3 plan (conflict from ' + this.deviceName() + ', 24 Nov 2026).dwg'
    this.history.unshift(t.id)
    this.at(2, () =>
      this.notify('conflict', 'Saved a second copy of Level 3 plan.dwg', 'Someone changed it on the drive while you were saving. Both versions are kept.', {
        label: 'Show', open: { target: 'web', path: t.conflict_copy ?? undefined },
      }),
    )
  }

  private deviceName() {
    return this.settings.device_name || 'ADA-PC'
  }

  // ---------- simulation ----------

  private canMove(): boolean {
    return !!this.account && this.mount.state === 'mounted' && !this.paused && this.online && this.sim >= this.restartingUntil && this.sim >= this.startingUntil
  }

  private waitReason(): CodeMessage | null {
    if (!this.online) return { code: 'offline', message: "The drive can't be reached. This file will go up when you're back online." }
    if (this.sim < this.restartingUntil) return { code: 'server_restarting', message: 'The drive is restarting. This file will carry on in a moment.' }
    return null
  }

  private moveFiles(dt: number) {
    if (!this.canMove()) {
      const reason = this.waitReason()
      for (let i = 0; i < this.queue.length; i++) {
        const t = this.transfers.get(this.queue[i])
        if (!t) continue
        if (i >= SLOTS && t.state !== 'active') continue
        if (t.state === 'active' || (reason && !t.error && t.state === 'waiting')) {
          t.state = 'waiting'
          t.bps = 0
          t.eta_seconds = null
          // Resume from the last finished part.
          t.bytes_done = Math.floor(t.bytes_done / PART) * PART
          t.error = reason ? { ...reason, retry_at: this.iso(Math.max(1, this.restartingUntil - this.sim)) } : null
          this.emit('transfer', this.publicTransfer(t))
        }
      }
      this.rate = 0
      this.publishSummary()
      return
    }
    const limit = this.settings.bandwidth.up_bps
    const base = 11.8 * MB * (0.86 + 0.28 * this.random())
    const rate = limit > 0 ? Math.min(limit, base) : base
    this.rate = this.rate === 0 ? rate : this.rate * 0.7 + rate * 0.3

    // Fill free slots in queue order.
    let active = this.queue.map((id) => this.transfers.get(id)!).filter((t) => t.state === 'active')
    for (const id of this.queue) {
      if (active.length >= SLOTS) break
      const t = this.transfers.get(id)!
      if (t.state !== 'waiting') continue
      t.error = null
      if (t.skipAs) {
        this.finish(t, 'skipped')
        continue
      }
      t.state = 'active'
      t.attempts += 1
      t.started_at = t.started_at ?? this.iso()
      active.push(t)
    }
    active = active.filter((t) => t.state === 'active')
    if (active.length === 0) {
      this.publishSummary()
      return
    }
    const share = (rate * dt) / active.length
    for (const t of active) {
      let budget = share
      let time = dt
      if (t.overhead > 0) {
        const used = Math.min(t.overhead, time)
        t.overhead -= used
        time -= used
        budget = (budget * time) / dt
      }
      const before = t.bytes_done
      t.bytes_done = Math.min(t.size, t.bytes_done + budget)
      this.workDone += t.bytes_done - before
      t.bps = Math.round(share / dt)
      t.eta_seconds = t.bps > 0 ? Math.ceil((t.size - t.bytes_done) / t.bps) : null
      if (t.bytes_done >= t.size && t.overhead <= 0) this.finish(t, 'done')
      else this.emit('transfer', this.publicTransfer(t))
    }
    this.publishSummary()
  }

  private finish(t: Internal, state: 'done' | 'skipped') {
    const wasLast = this.queue.length === 1
    t.state = state
    t.bps = 0
    t.eta_seconds = null
    t.finished_at = this.iso()
    if (state === 'skipped') {
      t.skipped_reason = t.skipAs
      this.workDone += t.size - t.bytes_done
    }
    t.bytes_done = t.size
    t.can = { retry: false, cancel: false }
    this.queue = this.queue.filter((id) => id !== t.id)
    this.history.unshift(t.id)
    if (this.history.length > HISTORY_LIMIT) {
      for (const old of this.history.splice(HISTORY_LIMIT)) this.transfers.delete(old)
    }
    this.emit('transfer', this.publicTransfer(t))
    if (wasLast && this.queue.length === 0) {
      const files = this.history.filter((id) => this.transfers.get(id)?.state === 'done').length
      this.workTotal = 0
      this.workDone = 0
      this.notify('finished', 'Everything is on the drive', plural(files, 'file') + ' sent.', { label: 'Open W:', open: { target: 'drive', path: '/' } })
    }
  }

  // ---------- state ----------

  private issues(): Issue[] {
    const issues: Issue[] = []
    if (!this.account) return issues
    if (this.held.length) {
      issues.push({ code: 'held_files', message: plural(this.held.length, 'file') + " couldn't be saved to the drive.", action: 'show_held' })
    }
    if (this.driver.state === 'restart_needed') {
      issues.push({ code: 'driver_restart_needed', message: 'WinFsp is installed. Sign out of Windows and back in (or restart) to finish, then the drive connects.', action: '' })
    }
    if (this.driver.state === 'missing' || this.driver.state === 'outdated') {
      issues.push({ code: 'driver_' + this.driver.state, message: 'Olsyn Drive needs a small Windows component (WinFsp) before W: can appear.', action: 'install_driver' })
    } else if (this.mount.state === 'failed') {
      issues.push({ code: 'mount_failed', message: this.mount.error?.message ?? "The drive couldn't be connected to a letter.", action: 'mount' })
    }
    if (this.update.required) issues.push({ code: 'update_required', message: 'This version is too old for the drive. Install the update to carry on.', action: 'apply_update' })
    if (this.update.state === 'failed') issues.push({ code: 'update_failed', message: "The last update didn't install. Olsyn Drive is still on " + this.version + '.', action: 'check_update' })
    return issues
  }

  private connection(): State['connection'] {
    if (!this.account) return this.signin?.state === 'waiting' ? 'linking' : 'signed_out'
    if (this.sim < this.startingUntil) return 'starting'
    if (!this.online) return 'offline'
    if (this.sim < this.restartingUntil) return 'server_restarting'
    if (this.issues().length) return 'attention'
    if (this.paused) return 'paused'
    if (this.queue.length) return 'working'
    return 'connected'
  }

  summary(): Summary {
    const active = this.queue.filter((id) => this.transfers.get(id)?.state === 'active').length
    const left = Math.max(0, this.workTotal - this.workDone)
    const moving = this.canMove() && this.queue.length > 0
    return {
      active,
      waiting: this.queue.length - active,
      held: this.held.length,
      files_left: this.queue.length,
      bytes_total: Math.round(this.workTotal),
      bytes_done: Math.round(Math.min(this.workDone, this.workTotal)),
      up_bps: moving ? Math.round(this.rate) : 0,
      down_bps: 0,
      eta_seconds: moving && this.rate > 0 ? Math.round(left / this.rate + this.queue.length * PER_FILE_OVERHEAD / SLOTS) : null,
    }
  }

  private stateWithoutSummary(): Omit<State, 'transfers'> {
    return {
      api_version: 1,
      api_versions: [1],
      agent_run: this.run,
      version: this.version,
      platform: 'windows-x64',
      ready: true,
      connection: this.connection(),
      reason: this.account ? null : this.reason,
      issues: this.issues(),
      account: this.account,
      device_name: this.deviceName(),
      signin: this.signin,
      mount: { ...this.mount, letter_confirmed: this.letterConfirmed },
      paused: this.paused,
      paused_until: this.pausedUntil ? new Date(this.pausedUntil).toISOString() : null,
      update: this.update,
      driver: this.driver,
      started_at: this.startedAt,
      first_run: this.account === null && this.scenario === 'first-run',
      unsent: { files: this.queue.length + this.held.length, bytes: Math.max(0, this.workTotal - this.workDone), held: this.held.length },
      cache: this.cacheState(),
    }
  }

  private cacheState() {
    const unsent = this.account ? Math.max(0, this.workTotal - this.workDone) : 0
    const uploaded = this.account ? this.cacheUploaded : 0
    const opened = this.account ? this.cacheOpened : 0
    return { used_bytes: unsent + uploaded + opened, cap_bytes: this.settings.cache_cap_bytes, unsent_bytes: unsent, uploaded_bytes: uploaded, opened_bytes: opened }
  }

  state(): State {
    return { ...this.stateWithoutSummary(), transfers: this.summary() }
  }

  private publishState() {
    const s = this.stateWithoutSummary()
    const json = JSON.stringify(s)
    if (json !== this.lastStateJson) {
      this.lastStateJson = json
      this.emit('state', this.state())
    }
    this.publishSummary()
  }

  private publishSummary() {
    const summary = this.summary()
    const json = JSON.stringify(summary)
    if (json !== this.lastSummaryJson) {
      this.lastSummaryJson = json
      this.emit('summary', summary)
    }
  }

  private publicTransfer(t: Internal): Transfer {
    const { skipAs: _skip, overhead: _overhead, ...rest } = t
    return { ...rest, bytes_done: Math.round(rest.bytes_done) }
  }

  private addActivity(kind: string, title: string, body: string, action: AgentNotification['action'], offsetSeconds = 0): AgentNotification {
    const n: AgentNotification = { id: 'n_' + this.notificationSeq++, kind, title, body, at: this.iso(offsetSeconds), action }
    this.activity.unshift(n)
    this.activity = this.activity.slice(0, 200)
    return n
  }

  private notify(kind: string, title: string, body: string, action: AgentNotification['action']) {
    const n = this.addActivity(kind, title, body, action)
    const wanted: Record<string, boolean> = {
      finished: this.settings.notifications.finished,
      held: this.settings.notifications.held,
      conflict: this.settings.notifications.held,
      signed_out: this.settings.notifications.signed_out,
      update_applied: this.settings.notifications.update_applied,
    }
    if (wanted[kind] !== false) this.emit('notification', n)
  }

  // ---------- events ----------

  private emit(event: string, data: unknown) {
    this.seq += 1
    const message: SSEMessage = { id: this.run + '-' + this.seq, event, data: JSON.stringify(data) }
    this.log.push(message)
    if (this.log.length > LOG_LIMIT) this.log.splice(0, this.log.length - LOG_LIMIT)
    for (const s of [...this.subscribers]) s.onMessage(message)
  }

  private snapshotMessage(lagged = false): SSEMessage {
    const active = this.queue.slice(0, 50).map((id) => this.publicTransfer(this.transfers.get(id)!))
    const held = this.held.map((id) => this.publicTransfer(this.transfers.get(id)!))
    this.seq += 1
    return { id: this.run + '-' + this.seq, event: 'snapshot', data: JSON.stringify({ state: this.state(), active, held, lagged }) }
  }

  /** GET /v1/events. */
  subscribe(lastEventId: string | null, subscriber: MockSubscriber): () => void {
    let open = true
    const close = () => {
      open = false
      this.subscribers.delete(subscriber)
    }
    const connect = () => {
      if (!open) return
      if (this.agentDown || this.downAttempts > 0) {
        if (this.downAttempts > 0) this.downAttempts -= 1
        open = false
        subscriber.onError(new Error('The agent is not running.'))
        return
      }
      subscriber.onOpen()
      const replayFrom = lastEventId && lastEventId.startsWith(this.run + '-') ? this.log.findIndex((m) => m.id === lastEventId) : -1
      if (replayFrom >= 0) {
        for (const m of this.log.slice(replayFrom + 1)) subscriber.onMessage(m)
      } else {
        subscriber.onMessage(this.snapshotMessage())
      }
      this.subscribers.add(subscriber)
    }
    if (this.snapshotDelay > 0) {
      const delay = this.snapshotDelay
      this.snapshotDelay = 0
      setTimeout(connect, delay * 1000)
    } else {
      queueMicrotask(connect)
    }
    return close
  }

  private dropSubscribers(message: string) {
    for (const s of [...this.subscribers]) {
      this.subscribers.delete(s)
      s.onError(new Error(message))
    }
  }

  get isDown() {
    return this.agentDown || this.downAttempts > 0
  }

  // ---------- requests ----------

  async handle(method: string, rawPath: string, body?: unknown): Promise<MockResponse> {
    if (this.isDown) {
      if (this.downAttempts > 0) this.downAttempts -= 1
      throw new Error('unreachable')
    }
    const url = new URL(rawPath, 'http://olsyn-drive')
    const path = url.pathname
    const b = (body ?? {}) as Record<string, unknown>
    const key = method + ' ' + path
    const result = this.route(key, path, url.searchParams, b)
    // Requests change state: publish it now rather than on the next tick.
    this.publishState()
    return result
  }

  private route(key: string, path: string, q: URLSearchParams, b: Record<string, unknown>): MockResponse {
    switch (key) {
      case 'GET /v1/state':
        return ok(this.state())
      case 'POST /v1/signin':
        return this.startSignIn()
      case 'POST /v1/signin/cancel':
        if (!this.signin || this.signin.state !== 'waiting') return err(409, 'not_signed_in', 'No sign-in is in progress.')
        this.signin = { ...this.signin, state: 'cancelled' }
        this.emit('signin', this.signin)
        return ok(this.signin)
      case 'POST /v1/signout':
        return this.signOut(b)
      case 'GET /v1/transfers':
        return this.listTransfers(q)
      case 'POST /v1/pause': {
        if (!this.account) return err(409, 'not_signed_in', 'Sign in first.')
        this.paused = true
        const minutes = typeof b.minutes === 'number' ? b.minutes : null
        this.pausedUntil = minutes ? this.wallMs() + minutes * 60_000 : null
        this.publishState()
        return ok(this.state())
      }
      case 'POST /v1/resume':
        this.paused = false
        this.pausedUntil = null
        this.publishState()
        return ok(this.state())
      case 'POST /v1/send':
        return this.send(b)
      case 'GET /v1/settings':
        return ok(this.settingsResponse())
      case 'PATCH /v1/settings':
        return this.patchSettings(b)
      case 'POST /v1/mount':
        return this.doMount(typeof b.letter === 'string' ? b.letter : undefined)
      case 'POST /v1/unmount':
        this.mount = { ...this.mount, state: 'unmounted', path: null }
        return ok({ mount: this.mount }, 202)
      case 'GET /v1/update':
        return ok(this.update)
      case 'POST /v1/update/check':
        return this.checkUpdate()
      case 'POST /v1/update/apply':
        return this.applyUpdate(b)
      case 'POST /v1/driver/install':
        return this.installDriver()
      case 'POST /v1/diagnostics':
        return ok({ path: localPath('C:', 'Users', 'ada', 'Downloads', 'olsyn-drive-diagnostics-20261124T031507Z.zip'), size: 482113 })
      case 'POST /v1/open':
        if (b.target === 'drive' && this.mount.state !== 'mounted') return err(409, 'not_mounted', 'The drive is not connected to a letter.')
        return { status: 204, body: null }
      case 'POST /v1/quit':
        if (this.mount.state === 'mounted' && b.unmount !== true) return err(409, 'mounted', 'Quitting disconnects the drive. Confirm first.')
        this.agentDown = true
        this.dropSubscribers('The agent quit.')
        return ok({ quitting: true }, 202)
      case 'GET /v1/cache':
        if (!this.account) return err(409, 'not_signed_in', 'Sign in first.')
        return ok(this.cacheUsage())
      case 'POST /v1/cache/free': {
        if (!this.account) return err(409, 'not_signed_in', 'Sign in first.')
        const freed = this.cacheUploaded + this.cacheOpened
        this.cacheUploaded = 0
        this.cacheOpened = 0
        return ok({ freed_files: freed > 0 ? 37 : 0, freed_bytes: freed, cache: this.cacheUsage() })
      }
      case 'GET /v1/drive/folders':
        return this.driveFolders(q.get('parent') || '/')
      case 'GET /v1/activity': {
        const limit = Math.min(200, Math.max(1, Number(q.get('limit') ?? 50) || 50))
        const start = Number((q.get('cursor') ?? 'a_0').replace('a_', '')) || 0
        const items = this.activity.slice(start, start + limit)
        return ok({ items, next_cursor: start + limit < this.activity.length ? 'a_' + (start + limit) : null })
      }
      case 'GET /v1/batches':
        return ok({ items: [] })
      case 'GET /v1/licences':
        return ok({ text: 'Olsyn Drive: open-source notices (mock).' })
    }
    const m = /^\/v1\/(transfers|held)\/([^/]+)\/([a-z-]+)$/.exec(path)
    if (m && key.startsWith('POST ')) return this.itemAction(m[1], decodeURIComponent(m[2]), m[3], b)
    if (path.startsWith('/v1/')) return err(404, 'not_found', 'Unknown endpoint.')
    return err(404, 'not_found', 'Unknown endpoint.')
  }

  private startSignIn(): MockResponse {
    if (this.account) return err(409, 'already_signed_in', 'Already signed in.')
    if (this.signin?.state === 'waiting') return ok(this.signin, 202)
    const letters = 'BCDFGHJKLMNPQRSTVWXZ'
    const pick = () => letters[Math.floor(this.random() * letters.length)]
    const code = [pick(), pick(), pick(), pick()].join('') + '-' + [pick(), pick(), pick(), pick()].join('')
    const expiresIn = this.scenario === 'link-expired' ? 20 : 600
    this.signin = {
      state: 'waiting',
      user_code: code,
      verification_url: this.settings.server_url + '/link?code=' + code,
      expires_at: this.iso(expiresIn),
      error: null,
    }
    this.reason = null
    this.emit('signin', this.signin)
    if (this.scenario !== 'link-expired') {
      const current = this.signin
      this.at(7, () => {
        if (this.signin !== current || current.state !== 'waiting') return
        this.signin = { ...current, state: 'approved' }
        this.emit('signin', this.signin)
        this.signInAs()
        this.account = { ...this.account!, linked_at: this.iso() }
        this.startingUntil = this.sim + 1.5
        this.addActivity('signed_in', 'Signed in as Ada Example', 'This PC is linked to your drive.', null)
      })
    }
    return ok(this.signin, 202)
  }

  private signOut(b: Record<string, unknown>): MockResponse {
    if (!this.account) return err(409, 'not_signed_in', 'Not signed in.')
    const unsentFiles = this.queue.length + this.held.length
    const unsentBytes = Math.max(0, this.workTotal - this.workDone)
    if (b.clear_cache === true && unsentFiles > 0 && b.discard_unsent_confirmed !== true) {
      return err(409, 'unsent_data', plural(unsentFiles, 'file') + " haven't reached the drive yet.", {
        unsent: { files: unsentFiles, bytes: unsentBytes, held: this.held.length },
      })
    }
    this.account = null
    this.reason = { code: 'signed_out', message: 'You signed out on this PC.' }
    this.signin = null
    this.mount = { ...this.mount, state: 'unmounted', path: null }
    for (const id of this.queue) {
      const t = this.transfers.get(id)
      if (t && t.state === 'active') t.state = 'waiting'
    }
    this.addActivity('signed_out', 'Signed out', 'This PC is no longer linked to your drive.', null)
    return ok({
      signed_out: true,
      unlinked: true,
      cache_cleared: b.clear_cache === true,
      unsent_kept: b.clear_cache === true ? null : { files: unsentFiles, bytes: unsentBytes },
    })
  }

  private counts(): TransferCounts {
    const c: TransferCounts = { active: 0, waiting: 0, held: this.held.length, done: 0, skipped: 0, failed: 0 }
    for (const id of this.queue) {
      const t = this.transfers.get(id)
      if (t?.state === 'active') c.active += 1
      else c.waiting += 1
    }
    for (const id of this.history) {
      const t = this.transfers.get(id)
      if (t?.state === 'done') c.done += 1
      else if (t?.state === 'skipped') c.skipped += 1
      else if (t?.state === 'failed') c.failed += 1
    }
    return c
  }

  private listTransfers(q: URLSearchParams): MockResponse {
    const states = (q.getAll('state').join(',') || 'active,waiting,held,done,skipped,failed').split(',').filter(Boolean) as ListState[]
    const allowed = new Set(['active', 'waiting', 'held', 'done', 'skipped', 'failed'])
    if (states.some((s) => !allowed.has(s))) return err(400, 'bad_request', 'Unknown state.')
    const limit = Math.min(500, Math.max(1, Number(q.get('limit') || 100)))
    const cursor = q.get('cursor')
    const start = cursor ? Number(cursor.replace('c_', '')) || 0 : 0
    const wanted = new Set(states)
    const ordered = [...this.queue, ...this.held, ...this.history]
      .map((id) => this.transfers.get(id)!)
      .filter((t) => t && wanted.has(t.state as ListState))
    const page = ordered.slice(start, start + limit).map((t) => this.publicTransfer(t))
    const next = start + limit < ordered.length ? 'c_' + (start + limit) : null
    return ok({ items: page, next_cursor: next, counts: this.counts() })
  }

  private itemAction(kind: string, id: string, action: string, b: Record<string, unknown>): MockResponse {
    const t = this.transfers.get(id)
    if (!t) return err(404, 'not_found', "That item isn't there any more.")
    if (kind === 'held') {
      if (t.state !== 'held') return err(409, 'not_held', 'That file is not held.')
      if (action === 'discard') {
        this.removeHeld(t)
        this.addActivity('held', 'Discarded ' + t.name, 'The copy on this PC was deleted.', null)
        return ok({ discarded: true })
      }
      if (action === 'reupload') {
        if (!t.held?.actions.includes('reupload')) return err(409, 'not_retryable', 'That file cannot be uploaded again from this PC.')
        this.held = this.held.filter((x) => x !== id)
        this.emit('held', { op: 'removed', item: this.publicTransfer({ ...t, state: 'waiting' }) })
        t.state = 'waiting'
        t.held = null
        t.bytes_done = 0
        this.queue.push(id)
        this.workTotal += t.size
        this.emit('transfer', this.publicTransfer(t))
        return ok({ queued: true })
      }
      if (action === 'save-copy') {
        const dir = typeof b.dest_dir === 'string' && b.dest_dir ? b.dest_dir : localPath('C:', 'Users', 'ada', 'Downloads')
        this.removeHeld(t)
        const saved = dir + String.fromCharCode(92) + t.name
        this.addActivity('held', 'Saved a copy of ' + t.name, saved, { label: 'Show', open: { target: 'folder', path: dir } })
        return ok({ path: saved })
      }
    }
    if (kind === 'transfers') {
      if (action === 'retry') {
        if (!t.can.retry) return err(409, 'not_retryable', 'That file cannot be tried again.')
        t.state = 'waiting'
        t.error = null
        t.can = { ...t.can, retry: false }
        this.history = this.history.filter((x) => x !== id)
        this.queue.push(id)
        this.emit('transfer', this.publicTransfer(t))
        return ok(this.publicTransfer(t))
      }
      if (action === 'cancel') {
        if (!t.can.cancel || (t.state !== 'active' && t.state !== 'waiting')) return err(409, 'not_cancellable', 'Files saved on the drive letter cannot be cancelled.')
        t.state = 'cancelled'
        t.finished_at = this.iso()
        this.workTotal -= t.size
        this.workDone -= t.bytes_done
        this.queue = this.queue.filter((x) => x !== id)
        this.emit('transfer', this.publicTransfer(t))
        return ok(this.publicTransfer(t))
      }
    }
    return err(404, 'not_found', 'Unknown action.')
  }

  private removeHeld(t: Internal) {
    this.held = this.held.filter((x) => x !== t.id)
    t.state = 'cancelled'
    this.emit('held', { op: 'removed', item: this.publicTransfer(t) })
    this.transfers.delete(t.id)
  }

  private send(b: Record<string, unknown>): MockResponse {
    if (!this.account) return err(409, 'not_signed_in', 'Sign in first.')
    const locals = Array.isArray(b.local_paths) ? b.local_paths.filter((p): p is string => typeof p === 'string') : []
    const local = typeof b.local_path === 'string' ? b.local_path : (locals[0] ?? '')
    const folder = typeof b.drive_folder === 'string' ? b.drive_folder : ''
    if (!local) return err(422, 'invalid', 'Choose something to send.', { field: 'local_path' })
    if (!(folder in driveTree)) return err(404, 'drive_folder_not_found', "That folder isn't on the drive.")
    if (readOnlyFolders.has(folder)) return err(502, 'drive_error', "You can't add files to that folder.", { drive_error_code: 'forbidden' })
    const batch = 'b_' + this.nextId
    const parts = local.split(/[\\/]/).filter(Boolean)
    const top = parts[parts.length - 1] ?? 'Folder'
    const files = smallFiles(40, this.random).map((f) => ({ ...f, skip: null, drivePath: folder.replace(/\/$/, '') + '/' + top + '/' + f.name }))
    let bytes = 0
    for (const f of files) {
      const t = this.makeTransfer(f, 'send', 'waiting', batch)
      t.local_path = local + String.fromCharCode(92) + f.name
      this.queue.push(t.id)
      this.workTotal += f.size
      bytes += f.size
    }
    this.addActivity('finished', 'Sending ' + top, plural(files.length, 'file') + ' to ' + folder + '.', null)
    return ok({ batch_id: batch, files: files.length, bytes }, 202)
  }

  private driveFolders(path: string): MockResponse {
    const names = driveTree[path]
    if (!names) return err(404, 'drive_folder_not_found', "That folder isn't on the drive.")
    const prefix = path === '/' ? '' : path
    return ok({
      parent: path,
      parent_id: 'f_' + path,
      can_write: !readOnlyFolders.has(path),
      folders: names.map((name) => {
        const p = prefix + '/' + name
        const ro = readOnlyFolders.has(p)
        return { id: 'f_' + p, name, path: p, can_write: !ro, why: ro ? (p.includes('managed') ? 'managed' : 'read_only') : '' }
      }),
      next_cursor: null,
    })
  }

  private cacheUsage() {
    const c = this.cacheState()
    return {
      dir: localPath('C:', 'Users', 'ada', 'AppData', 'Local', 'Olsyn Drive', 'Cache'),
      ...c,
      unsent_files: this.queue.length + this.held.length,
      disk_free_bytes: 212 * GB,
    }
  }

  private settingsResponse(): SettingsResponse {
    const inUse = { ...this.lettersInUse }
    const ours = this.mount.state === 'mounted' ? this.mount.letter : null
    if (ours) delete inUse[ours]
    const all = Array.from({ length: 23 }, (_, i) => String.fromCharCode(68 + i) + ':')
    const letters = all.filter((l) => !inUse[l] || l === ours).sort()
    return {
      settings: this.settings,
      defaults: { ...this.settings, drive_letter: 'W:', cache_cap_bytes: 10 * GB, bandwidth: { up_bps: 0, down_bps: 0 }, start_at_login: true, update_channel: 'stable' },
      choices: { drive_letters: letters, letters_in_use: inUse, suggested_letter: suggestLetter(this.settings.drive_letter, inUse), update_channels: ['stable', 'beta'] },
      pending_restart: [],
    }
  }

  private patchSettings(b: Record<string, unknown>): MockResponse {
    const next: Settings = JSON.parse(JSON.stringify(this.settings))
    for (const [key, value] of Object.entries(b)) {
      const current = (next as unknown as Record<string, unknown>)[key]
      if (current !== null && typeof current === 'object' && value && typeof value === 'object') {
        Object.assign(current as object, value)
      } else {
        ;(next as unknown as Record<string, unknown>)[key] = value
      }
    }
    if (!/^[D-Z]:$/.test(next.drive_letter)) return err(422, 'invalid_setting', 'Pick a letter from the list.', { field: 'drive_letter' })
    if (next.cache_cap_bytes < GB) return err(422, 'invalid_setting', 'The cache needs at least 1 GB.', { field: 'cache_cap_bytes' })
    if (next.bandwidth.up_bps < 0 || next.bandwidth.down_bps < 0) return err(422, 'invalid_setting', 'A limit cannot be below zero.', { field: 'bandwidth' })
    const letterChanged = next.drive_letter !== this.settings.drive_letter
    const pending: string[] = []
    if (next.cache_dir !== this.settings.cache_dir) pending.push('cache_dir')
    if (next.device_name !== this.settings.device_name) pending.push('device_name')
    this.settings = next
    if (letterChanged) {
      this.mount = { ...this.mount, letter: next.drive_letter, path: this.mount.state === 'mounted' ? next.drive_letter + String.fromCharCode(92) : null }
    }
    this.emit('settings', this.settings)
    return ok({ ...this.settingsResponse(), pending_restart: pending })
  }

  private doMount(letter?: string): MockResponse {
    if (!this.account) return err(409, 'not_signed_in', 'Sign in first.')
    if (this.driver.state === 'missing' || this.driver.state === 'installing') return err(409, 'driver_missing', 'Install the driver first.')
    if (this.driver.state === 'restart_needed') return err(409, 'driver_restart_needed', 'Sign out of Windows and back in to finish adding WinFsp.')
    if (this.mount.state === 'mounted') return ok({ mount: this.mount })
    if (letter !== undefined) {
      const l = letter.toUpperCase().replace(/:?$/, ':')
      if (!/^[D-Z]:$/.test(l)) return err(422, 'invalid', 'Choose a letter from D: to Z:.', { field: 'letter' })
      const kind = this.lettersInUse[l]
      if (kind) return err(409, 'letter_taken', l + ' is already used on this PC by ' + (kind === 'network' ? 'a network drive' : 'another drive') + '. Choose another letter.', { letter: l, kind })
      if (l !== this.settings.drive_letter) {
        this.settings = { ...this.settings, drive_letter: l }
        this.emit('settings', this.settings)
      }
    }
    this.letterConfirmed = true
    this.mount = { state: 'mounting', letter: this.settings.drive_letter, path: null, error: null }
    this.at(1.2, () => {
      this.mounted()
      this.addActivity('mounted', this.settings.drive_letter + ' is connected', 'Your drive shows up in File Explorer.', null)
    })
    return ok({ mount: this.mount }, 202)
  }

  private installDriver(): MockResponse {
    if (this.driver.state === 'restart_needed') return err(409, 'driver_restart_needed', 'Sign out of Windows and back in to finish adding WinFsp.')
    if (this.driver.state === 'installing') return ok(this.driver, 202)
    this.driver = { ...this.driver, state: 'installing', progress: 'preparing', error: null }
    this.at(0.8, () => (this.driver = { ...this.driver, progress: 'approval' }))
    if (this.scenario === 'driver-declined' && !this.declinedOnce) {
      this.declinedOnce = true
      this.at(2.5, () => {
        this.driver = { state: 'missing', name: 'winfsp', version: null, min_version: '2.0', error: { code: 'declined', message: 'The administrator prompt was declined.' } }
      })
      return ok(this.driver, 202)
    }
    this.at(2.3, () => (this.driver = { ...this.driver, progress: 'installing' }))
    if (this.scenario === 'driver-restart') {
      this.at(4, () => {
        this.driver = { state: 'restart_needed', name: 'winfsp', version: '2.2.26323', min_version: '2.0', error: { code: 'restart_needed', message: 'WinFsp is installed. Sign out of Windows and back in (or restart) to finish.' } }
      })
      return ok(this.driver, 202)
    }
    this.at(4, () => {
      this.driver = { state: 'ok', name: 'winfsp', version: '2.2.26323', min_version: '2.0', error: null }
      if (this.account && this.mount.state === 'failed') {
        this.mount = { ...this.mount, state: 'unmounted', error: null }
        this.doMount()
      }
    })
    return ok(this.driver, 202)
  }

  private checkUpdate(): MockResponse {
    if (this.update.state === 'available' || this.update.state === 'ready') return ok(this.update)
    this.update = { ...this.update, state: 'up_to_date', checked_at: this.iso() }
    this.emit('update', this.update)
    return ok(this.update)
  }

  private applyUpdate(b: Record<string, unknown>): MockResponse {
    if (!this.update.available) return err(409, 'no_update', 'There is no update to install.')
    const total = 18 * MB
    this.update = { ...this.update, state: 'downloading', apply: b.when === 'now' ? 'now' : 'when_idle', progress: { bytes_done: 0, bytes_total: total } }
    this.emit('update', this.update)
    const steps = 8
    for (let i = 1; i <= steps; i++) {
      this.at(i * 0.5, () => {
        this.update = { ...this.update, progress: { bytes_done: Math.round((total * i) / steps), bytes_total: total } }
        if (i === steps) this.update = { ...this.update, state: 'ready' }
        this.emit('update', this.update)
      })
    }
    this.at(steps * 0.5 + 1.5, () => {
      this.update = { ...this.update, state: 'applying' }
      this.emit('update', this.update)
    })
    this.at(steps * 0.5 + 2.5, () => this.restartAgent())
    return ok(this.update, 202)
  }

  /** Simulates the swap: the stream drops, a new agent_run answers with the new version. */
  private restartAgent() {
    this.agentDown = true
    this.dropSubscribers('The agent is restarting for an update.')
    this.at(2, () => {
      const next = this.update.available ?? this.version
      this.version = next
      this.run = 'r' + Math.random().toString(36).slice(2, 8)
      this.log = []
      this.seq = 0
      this.startedAt = this.iso()
      this.update = { ...this.update, state: 'up_to_date', current: next, available: null, notes: null, progress: null, checked_at: this.iso() }
      this.agentDown = false
      this.at(1, () => this.notify('update_applied', 'Olsyn Drive is up to date', 'Version ' + next + ' is installed.', null))
    })
  }
}

function plural(n: number, word: string) {
  return n.toLocaleString('en') + ' ' + word + (n === 1 ? '' : 's')
}
