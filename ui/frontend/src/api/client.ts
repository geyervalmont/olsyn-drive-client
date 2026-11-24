// Typed client for the agent's local API (docs/ipc.md v1).

import { AgentError, errorFromResponse } from './errors'
import type { SSEMessage } from './sse'
import type { StreamHandlers, Transport } from './transport'
import type {
  ActivityPage,
  AgentEventName,
  Batch,
  CacheFreed,
  CacheUsage,
  Driver,
  DriveFolderList,
  IncomingEvent,
  ListState,
  Mount,
  OpenRequest,
  SendResult,
  SettingsPatch,
  SettingsResponse,
  SignIn,
  SignOutResult,
  State,
  Transfer,
  TransferPage,
  Update,
} from './types'

const knownEvents: ReadonlySet<string> = new Set<AgentEventName>([
  'snapshot',
  'state',
  'summary',
  'transfer',
  'held',
  'signin',
  'update',
  'settings',
  'notification',
  'batch',
])

/** Turns one SSE message into a typed event, or null for anything this UI does not know. */
export function parseEvent(message: SSEMessage): IncomingEvent | null {
  if (!knownEvents.has(message.event)) return null
  let data: unknown
  try {
    data = JSON.parse(message.data)
  } catch {
    return null
  }
  if (!data || typeof data !== 'object') return null
  return { event: message.event, data, id: message.id } as IncomingEvent
}

export interface TransferQuery {
  state?: ListState | ListState[]
  cursor?: string | null
  limit?: number
}

export interface EventSubscription {
  onOpen(): void
  onEvent(event: IncomingEvent): void
  onError(error: Error): void
}

export class AgentClient {
  constructor(private readonly transport: Transport) {}

  private async call<T>(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<T> {
    const response = await this.transport.request(method, path, body, signal)
    if (response.status >= 400) throw errorFromResponse(response.status, response.body)
    return response.body as T
  }

  state(signal?: AbortSignal) {
    return this.call<State>('GET', '/v1/state', undefined, signal)
  }

  // Sign-in
  signIn(openBrowser = true) {
    return this.call<SignIn>('POST', '/v1/signin', { open_browser: openBrowser })
  }
  cancelSignIn() {
    return this.call<SignIn>('POST', '/v1/signin/cancel', {})
  }
  signOut(options: { clearCache?: boolean; discardUnsentConfirmed?: boolean } = {}) {
    return this.call<SignOutResult>('POST', '/v1/signout', {
      clear_cache: options.clearCache ?? false,
      discard_unsent_confirmed: options.discardUnsentConfirmed ?? false,
    })
  }

  // Transfers and held files
  transfers(query: TransferQuery = {}, signal?: AbortSignal) {
    const params = new URLSearchParams()
    const states = Array.isArray(query.state) ? query.state : query.state ? [query.state] : []
    if (states.length) params.set('state', states.join(','))
    if (query.cursor) params.set('cursor', query.cursor)
    if (query.limit) params.set('limit', String(Math.min(500, Math.max(1, query.limit))))
    const qs = params.toString()
    return this.call<TransferPage>('GET', '/v1/transfers' + (qs ? '?' + qs : ''), undefined, signal)
  }
  retry(id: string) {
    return this.call<Transfer>('POST', '/v1/transfers/' + encodeURIComponent(id) + '/retry', {})
  }
  cancel(id: string) {
    return this.call<Transfer>('POST', '/v1/transfers/' + encodeURIComponent(id) + '/cancel', {})
  }
  discardHeld(id: string) {
    return this.call<{ discarded: boolean }>('POST', '/v1/held/' + encodeURIComponent(id) + '/discard', {})
  }
  /** Sends a held file again (only offered for a checksum mismatch). */
  reuploadHeld(id: string) {
    return this.call<{ queued: boolean }>('POST', '/v1/held/' + encodeURIComponent(id) + '/reupload', {})
  }
  saveHeldCopy(id: string, destDir?: string) {
    return this.call<{ path: string }>('POST', '/v1/held/' + encodeURIComponent(id) + '/save-copy', destDir ? { dest_dir: destDir } : {})
  }
  pause(minutes?: number) {
    return this.call<State>('POST', '/v1/pause', minutes ? { minutes } : {})
  }
  resume() {
    return this.call<State>('POST', '/v1/resume', {})
  }
  /** One batch: files and folders from this PC into one drive folder. */
  send(localPaths: string[], driveFolder: string) {
    return this.call<SendResult>('POST', '/v1/send', { local_paths: localPaths, drive_folder: driveFolder })
  }
  batches() {
    return this.call<{ items: Batch[] }>('GET', '/v1/batches')
  }
  /** Cancels a batch midway: what is not on the drive yet is not sent (agent 1.2). */
  cancelBatch(id: string) {
    return this.call<Batch>('POST', '/v1/batches/' + encodeURIComponent(id) + '/cancel', {})
  }

  // Settings
  settings() {
    return this.call<SettingsResponse>('GET', '/v1/settings')
  }
  patchSettings(patch: SettingsPatch) {
    return this.call<SettingsResponse>('PATCH', '/v1/settings', patch)
  }

  // Mount
  /** Connects the drive; letter (optional) is the person's choice, which also confirms it. */
  mount(letter?: string) {
    return this.call<{ mount: Mount }>('POST', '/v1/mount', letter ? { letter } : {})
  }
  unmount(force = false) {
    return this.call<{ mount: Mount }>('POST', '/v1/unmount', { force })
  }

  // Updates and driver
  update() {
    return this.call<Update>('GET', '/v1/update')
  }
  checkUpdate() {
    return this.call<Update>('POST', '/v1/update/check', {})
  }
  applyUpdate(when: 'idle' | 'now' = 'idle') {
    return this.call<Update>('POST', '/v1/update/apply', { when })
  }
  installDriver() {
    return this.call<Driver>('POST', '/v1/driver/install', {})
  }

  // Other
  diagnostics() {
    return this.call<{ path: string; size: number }>('POST', '/v1/diagnostics', {})
  }
  open(request: OpenRequest) {
    return this.call<null>('POST', '/v1/open', request)
  }
  quit(unmount: boolean) {
    return this.call<{ quitting: boolean }>('POST', '/v1/quit', { unmount })
  }

  // Added in agent 1.1 (docs/ipc.md v1, additive). Each degrades to null on 404 from an older agent so the UI can hide the feature.
  async cache(): Promise<CacheUsage | null> {
    return this.optional(() => this.call<CacheUsage>('GET', '/v1/cache'))
  }
  /** Clears every cached file the drive has (agent 1.2); null from an older agent. */
  async freeCache(): Promise<CacheFreed | null> {
    return this.optional(() => this.call<CacheFreed>('POST', '/v1/cache/free', {}))
  }
  async driveFolders(parent: string): Promise<DriveFolderList | null> {
    return this.optional(() => this.call<DriveFolderList>('GET', '/v1/drive/folders?parent=' + encodeURIComponent(parent)))
  }
  async activity(cursor?: string | null, limit = 50): Promise<ActivityPage | null> {
    const params = new URLSearchParams({ limit: String(limit) })
    if (cursor) params.set('cursor', cursor)
    return this.optional(() => this.call<ActivityPage>('GET', '/v1/activity?' + params.toString()))
  }
  /** The full open-source notice text (licence texts of every component). */
  async licences(): Promise<string | null> {
    const r = await this.optional(() => this.call<{ text: string }>('GET', '/v1/licences'))
    return r?.text ?? null
  }

  private async optional<T>(fn: () => Promise<T>): Promise<T | null> {
    try {
      return await fn()
    } catch (err) {
      if (err instanceof AgentError && (err.status === 404 || err.status === 405)) return null
      throw err
    }
  }

  /** GET /v1/events. Unknown event types are skipped. Returns a close function. */
  subscribe(lastEventId: string | null, subscription: EventSubscription): () => void {
    const handlers: StreamHandlers = {
      onOpen: () => subscription.onOpen(),
      onMessage: (message) => {
        const event = parseEvent(message)
        if (event) subscription.onEvent(event)
      },
      onError: (error) => subscription.onError(error),
    }
    return this.transport.stream(lastEventId, handlers)
  }
}
