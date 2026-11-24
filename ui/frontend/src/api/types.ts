// Types for the agent's local API, transcribed from docs/ipc.md (v1).

export type ISOTime = string

export type Connection =
  | 'signed_out'
  | 'linking'
  | 'starting'
  | 'offline'
  | 'server_restarting'
  | 'attention'
  | 'paused'
  | 'working'
  | 'connected'

export interface CodeMessage {
  code: string
  message: string
}

export type IssueAction =
  | 'show_held'
  | 'mount'
  | 'install_driver'
  | 'apply_update'
  | 'check_update'
  | 'show_settings'
  | 'sign_in'

export interface Issue {
  code: string
  message: string
  action: IssueAction | string | null
}

export interface Account {
  server: string
  user_id: string
  name: string
  email: string
  device_id: string
  read_only: boolean
  linked_at: ISOTime
}

export type SignInState = 'waiting' | 'approved' | 'denied' | 'expired' | 'cancelled' | 'failed'

export interface SignIn {
  state: SignInState
  user_code: string
  verification_url: string
  expires_at: ISOTime
  error: CodeMessage | null
}

export type MountState = 'unmounted' | 'mounting' | 'mounted' | 'unmounting' | 'failed'

export interface Mount {
  state: MountState
  letter: string | null
  path: string | null
  error: CodeMessage | null
}

export interface Summary {
  active: number
  waiting: number
  held: number
  files_left: number
  bytes_total: number
  bytes_done: number
  up_bps: number
  down_bps: number
  eta_seconds: number | null
}

export type UpdateState =
  | 'idle'
  | 'checking'
  | 'up_to_date'
  | 'available'
  | 'downloading'
  | 'ready'
  | 'applying'
  | 'failed'

export interface Update {
  state: UpdateState
  channel?: string
  current: string
  available: string | null
  notes: string | null
  published_at?: ISOTime | null
  required: boolean
  driver_required?: { name: string; min_version: string } | null
  progress: { bytes_done: number; bytes_total: number } | null
  error: CodeMessage | null
  checked_at: ISOTime | null
  apply?: 'when_idle' | 'now'
}

export type DriverState = 'ok' | 'missing' | 'outdated' | 'installing' | 'not_needed' | 'unknown'

export interface Driver {
  state: DriverState
  name: string
  version: string | null
  min_version: string | null
  error?: CodeMessage | null
}

export interface State {
  api_version: number
  api_versions: number[]
  agent_run: string
  version: string
  platform: string
  ready: boolean
  connection: Connection
  reason: CodeMessage | null
  issues: Issue[]
  account: Account | null
  device_name: string
  signin: SignIn | null
  mount: Mount
  paused: boolean
  paused_until: ISOTime | null
  transfers: Summary
  update: Update
  driver: Driver
  started_at: ISOTime
  /** True until this PC has been signed in once. Absent from agents older than 1.1. */
  first_run?: boolean
  /** What has not reached the drive yet. Absent from agents older than 1.1. */
  unsent?: Unsent
}

export interface Unsent {
  files: number
  bytes: number
  held: number
}

export type Direction = 'up' | 'down'
export type Origin = 'mount' | 'send' | 'read'
export type TransferState = 'active' | 'waiting' | 'held' | 'done' | 'skipped' | 'failed' | 'cancelled'
export type HeldAction = 'reupload' | 'save_copy' | 'discard'

export interface Held {
  code: string
  message: string
  since: ISOTime
  actions: HeldAction[]
}

export interface Transfer {
  id: string
  direction: Direction
  origin: Origin
  state: TransferState
  name: string
  local_path: string
  drive_path: string
  node_id: string | null
  size: number
  bytes_done: number
  bps: number
  eta_seconds: number | null
  attempts: number
  queued_at: ISOTime
  started_at: ISOTime | null
  finished_at: ISOTime | null
  error: (CodeMessage & { retry_at?: ISOTime | null }) | null
  held: Held | null
  skipped_reason: 'already_there' | 'junk' | string | null
  conflict_copy: string | null
  batch_id: string | null
  can: { retry: boolean; cancel: boolean }
}

export type ListState = 'active' | 'waiting' | 'held' | 'done' | 'skipped' | 'failed'

export interface TransferCounts {
  active: number
  waiting: number
  held: number
  done: number
  skipped: number
  failed: number
}

export interface TransferPage {
  items: Transfer[]
  next_cursor: string | null
  counts: TransferCounts
}

export interface Settings {
  drive_letter: string
  cache_dir: string
  cache_cap_bytes: number
  bandwidth: { up_bps: number; down_bps: number }
  start_at_login: boolean
  update_channel: string
  notifications: { finished: boolean; held: boolean; signed_out: boolean; update_applied: boolean }
  server_url: string
  device_name: string
}

export interface SettingsResponse {
  settings: Settings
  defaults: Settings
  choices: { drive_letters: string[]; update_channels: string[] }
  pending_restart: string[]
}

/** A JSON merge patch (RFC 7396) of Settings. */
export type SettingsPatch = {
  [K in keyof Settings]?: Settings[K] extends object ? Partial<Settings[K]> : Settings[K]
}

export type NotificationKind =
  | 'finished'
  | 'held'
  | 'conflict'
  | 'signed_out'
  | 'update_applied'
  | 'update_failed'
  | 'error'

export interface OpenRequest {
  target: 'drive' | 'web' | 'folder' | 'logs' | 'ui'
  path?: string
}

export interface AgentNotification {
  id: string
  kind: NotificationKind | string
  title: string
  body: string
  at: ISOTime
  action: { label: string; open: OpenRequest } | null
}

export interface SignOutResult {
  signed_out: boolean
  unlinked: boolean
  cache_cleared: boolean
  unsent_kept: { files: number; bytes: number } | null
  unlink_error?: string
}

export interface SendResult {
  batch_id: string
  files: number
  bytes: number
}

/** GET /v1/cache. */
export interface CacheUsage {
  dir: string
  used_bytes: number
  cap_bytes: number
  unsent_bytes: number
  unsent_files: number
  disk_free_bytes: number
}

export type FolderWhy = '' | 'read_only' | 'managed' | 'sealed'

/** GET /v1/drive/folders?parent=. */
export interface DriveFolderList {
  parent: string
  parent_id: string
  can_write: boolean
  folders: { id: string; name: string; path: string; can_write: boolean; why: FolderWhy | string }[]
  next_cursor: string | null
}

/** GET /v1/activity. Items reuse the Notification shape, newest first. */
export interface ActivityPage {
  items: AgentNotification[]
  next_cursor: string | null
}

export type BatchState = 'scanning' | 'sending' | 'done' | 'failed' | 'cancelled'

/** One POST /v1/send: the "batch" event and GET /v1/batches. */
export interface Batch {
  batch_id: string
  state: BatchState | string
  drive_folder: string
  local_paths: string[]
  files: number
  files_done: number
  files_skipped: number
  files_failed: number
  files_held: number
  files_not_local: number
  bytes: number
  bytes_done: number
  bps: number
  eta_seconds: number | null
  started_at: ISOTime
  finished_at: ISOTime | null
  error: CodeMessage | null
}

export interface Snapshot {
  state: State
  active: Transfer[]
  held: Transfer[]
  lagged: boolean
}

export type AgentEvent =
  | { event: 'snapshot'; data: Snapshot }
  | { event: 'state'; data: State }
  | { event: 'summary'; data: Summary }
  | { event: 'transfer'; data: Transfer }
  | { event: 'held'; data: { op: 'added' | 'removed'; item: Transfer } }
  | { event: 'signin'; data: SignIn }
  | { event: 'update'; data: Update }
  | { event: 'settings'; data: Settings }
  | { event: 'notification'; data: AgentNotification }
  | { event: 'batch'; data: Batch }

export type AgentEventName = AgentEvent['event']

/** An event as it arrives, with its SSE id. Unknown event names are dropped before this point. */
export type IncomingEvent = AgentEvent & { id: string | null }
