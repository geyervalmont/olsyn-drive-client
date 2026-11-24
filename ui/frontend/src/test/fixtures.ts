import type { State, Summary, Transfer } from '../api/types'

export const summary = (over: Partial<Summary> = {}): Summary => ({
  active: 0, waiting: 0, held: 0, files_left: 0, bytes_total: 0, bytes_done: 0, up_bps: 0, down_bps: 0, eta_seconds: null, ...over,
})

export const state = (over: Partial<State> = {}): State => ({
  api_version: 1,
  api_versions: [1],
  agent_run: 'r1',
  version: '1.4.2',
  platform: 'windows-x64',
  ready: true,
  connection: 'connected',
  reason: null,
  issues: [],
  account: { server: 'https://drive.olsyn.com', user_id: 'u', name: 'Ada Example', email: 'ada@example.com', device_id: 'd', read_only: false, linked_at: '2026-11-24T00:00:00.000Z' },
  device_name: 'ADA-PC',
  signin: null,
  mount: { state: 'mounted', letter: 'W:', path: 'W:\\', error: null },
  paused: false,
  paused_until: null,
  transfers: summary(),
  update: { state: 'up_to_date', current: '1.4.2', available: null, notes: null, required: false, progress: null, error: null, checked_at: null },
  driver: { state: 'ok', name: 'winfsp', version: '2.2', min_version: '2.0' },
  started_at: '2026-11-24T00:00:00.000Z',
  ...over,
})

export const transfer = (id: string, over: Partial<Transfer> = {}): Transfer => ({
  id,
  direction: 'up',
  origin: 'mount',
  state: 'waiting',
  name: id + '.jpg',
  local_path: 'W:\\Projects\\' + id + '.jpg',
  drive_path: '/Projects/' + id + '.jpg',
  node_id: null,
  size: 1000,
  bytes_done: 0,
  bps: 0,
  eta_seconds: null,
  attempts: 0,
  queued_at: '2026-11-24T00:00:00.000Z',
  started_at: null,
  finished_at: null,
  error: null,
  held: null,
  skipped_reason: null,
  conflict_copy: null,
  batch_id: null,
  can: { retry: false, cancel: false },
  ...over,
})
