import { describe, expect, it } from 'vitest'
import type { SSEMessage } from '../api/sse'
import type { SettingsResponse, State, Summary, Transfer, TransferPage } from '../api/types'
import { MockAgent } from './agent'

const NOW = Date.parse('2026-11-24T03:00:00.000Z')
const make = (scenario: ConstructorParameters<typeof MockAgent>[0]['scenario']) => new MockAgent({ scenario, now: () => NOW })

function listen(agent: MockAgent, lastId: string | null = null) {
  const messages: SSEMessage[] = []
  agent.subscribe(lastId, { onOpen: () => undefined, onMessage: (m) => messages.push(m), onError: () => undefined })
  return messages
}
const flush = () => new Promise((r) => setTimeout(r, 0))
const body = async <T,>(agent: MockAgent, method: string, path: string, b?: unknown) => (await agent.handle(method, path, b)).body as T

describe('MockAgent', () => {
  it('starts every stream with a snapshot and replays after Last-Event-ID', async () => {
    const agent = make('copy')
    const first = listen(agent)
    await flush()
    expect(first[0].event).toBe('snapshot')
    const snap = JSON.parse(first[0].data) as { state: State; active: Transfer[] }
    expect(snap.state.connection).toBe('working')
    expect(snap.active).toHaveLength(50)
    agent.tick(1)
    const lastId = first[first.length - 1].id
    agent.tick(1)
    const replay = listen(agent, lastId)
    await flush()
    expect(replay[0].event).not.toBe('snapshot')
    expect(replay[0].id).toBe(first[first.indexOf(first.find((m) => m.id === lastId)!) + 1].id)
  })

  it('copies 3,000 small files and a 3 GB file with sane progress, speed and time left', async () => {
    const agent = make('copy')
    const s0 = agent.summary()
    expect(s0.files_left).toBe(3001)
    expect(s0.bytes_total).toBeGreaterThan(3 * 1024 ** 3)
    for (let i = 0; i < 40; i++) agent.tick(0.25)
    const s1 = agent.summary()
    expect(s1.bytes_done).toBeGreaterThan(0)
    expect(s1.up_bps).toBeGreaterThan(5 * 1024 * 1024)
    expect(s1.up_bps).toBeLessThan(20 * 1024 * 1024)
    expect(s1.eta_seconds).toBeGreaterThan(100)
    expect(s1.active).toBeLessThanOrEqual(4)
    const big = (await body<TransferPage>(agent, 'GET', '/v1/transfers?state=active')).items.find((t) => t.size === 3 * 1024 ** 3)
    expect(big?.bytes_done).toBeGreaterThan(0)
    const counts = (await body<TransferPage>(agent, 'GET', '/v1/transfers?limit=1')).counts
    expect(counts.done + counts.skipped).toBeGreaterThan(0)
  })

  it('waits during a drive restart, then resumes from the last part', () => {
    const agent = make('restart')
    for (let i = 0; i < 15 * 4; i++) agent.tick(0.25)
    expect(agent.state().connection).toBe('server_restarting')
    const waitingSummary: Summary = agent.summary()
    expect(waitingSummary.active).toBe(0)
    expect(waitingSummary.up_bps).toBe(0)
    for (let i = 0; i < 17 * 4; i++) agent.tick(0.25)
    expect(agent.state().connection).toBe('working')
    expect(agent.summary().active).toBeGreaterThan(0)
  })

  it('holds four files with their reasons and lets them be re-uploaded, saved or discarded', async () => {
    const agent = make('held')
    const st = agent.state()
    expect(st.connection).toBe('attention')
    expect(st.issues[0]).toMatchObject({ code: 'held_files', action: 'show_held' })
    const held = (await body<TransferPage>(agent, 'GET', '/v1/transfers?state=held')).items
    expect(held.map((t) => t.held?.code).sort()).toEqual(['checksum_mismatch', 'forbidden', 'invalid_name', 'managed'])
    const saved = await body<{ path: string }>(agent, 'POST', '/v1/held/' + held[0].id + '/save-copy', { dest_dir: 'C:\\Temp' })
    expect(saved.path).toContain('C:\\Temp')
    expect((await body<{ discarded: boolean }>(agent, 'POST', '/v1/held/' + held[1].id + '/discard')).discarded).toBe(true)
    const again = await agent.handle('POST', '/v1/held/' + held[1].id + '/discard')
    expect(again.status).toBe(404)
    expect(agent.state().transfers.held).toBe(2)
    const damaged = held.find((t) => t.held?.code === 'checksum_mismatch')!
    const refused = held.slice(2).find((t) => t.held?.code !== 'checksum_mismatch')!
    expect((await agent.handle('POST', '/v1/held/' + refused.id + '/reupload')).status).toBe(409)
    expect((await body<{ queued: boolean }>(agent, 'POST', '/v1/held/' + damaged.id + '/reupload')).queued).toBe(true)
    expect(agent.state().transfers.held).toBe(1)
  })

  it('refuses a taken letter and confirms a free one', async () => {
    const agent = make('letter-taken')
    await body(agent, 'POST', '/v1/signin', { open_browser: false })
    agent.tick(8)
    expect(agent.state().mount.letter_confirmed).toBe(false)
    const set = await body<SettingsResponse>(agent, 'GET', '/v1/settings')
    expect(set.choices.suggested_letter).toBe('X:')
    expect(set.choices.letters_in_use).toMatchObject({ 'W:': 'network', 'O:': 'network' })
    expect(set.choices.drive_letters).not.toContain('W:')
    const taken = await agent.handle('POST', '/v1/mount', { letter: 'W:' })
    expect(taken.status).toBe(409)
    expect((await agent.handle('POST', '/v1/mount', { letter: 'x' })).status).toBe(202)
    agent.tick(2)
    expect(agent.state().mount).toMatchObject({ state: 'mounted', letter: 'X:', letter_confirmed: true })
  })

  it('signs in with a code, then approves', async () => {
    const agent = make('first-run')
    expect(agent.state().connection).toBe('signed_out')
    const s = await body<{ state: string; user_code: string }>(agent, 'POST', '/v1/signin', { open_browser: true })
    expect(s.state).toBe('waiting')
    expect(s.user_code).toMatch(/^[A-Z]{4}-[A-Z]{4}$/)
    expect(agent.state().connection).toBe('linking')
    agent.tick(8)
    expect(agent.state().account?.name).toBe('Ada Example')
    expect(agent.state().driver.state).toBe('missing')
    expect((await agent.handle('POST', '/v1/mount')).status).toBe(409)
  })

  it('refuses to clear unsent files on sign-out until confirmed', async () => {
    const agent = make('offline')
    const first = await agent.handle('POST', '/v1/signout', { clear_cache: true })
    expect(first.status).toBe(409)
    expect((first.body as { error: string }).error).toBe('unsent_data')
    const second = await agent.handle('POST', '/v1/signout', { clear_cache: true, discard_unsent_confirmed: true })
    expect(second.status).toBe(200)
    expect(agent.state().connection).toBe('signed_out')
  })

  it('applies an update: the stream drops and a new run answers with the new version', async () => {
    const agent = make('update')
    const errors: Error[] = []
    agent.subscribe(null, { onOpen: () => undefined, onMessage: () => undefined, onError: (e) => errors.push(e) })
    await flush()
    const runBefore = agent.state().agent_run
    await agent.handle('POST', '/v1/update/apply', { when: 'now' })
    for (let i = 0; i < 40; i++) agent.tick(0.25)
    expect(errors).toHaveLength(1)
    for (let i = 0; i < 12; i++) agent.tick(0.25)
    expect(agent.isDown).toBe(false)
    expect(agent.state().version).toBe('1.5.0')
    expect(agent.state().agent_run).not.toBe(runBefore)
  })

  it('validates settings as a whole', async () => {
    const agent = make('empty')
    const bad = await agent.handle('PATCH', '/v1/settings', { drive_letter: 'A:' })
    expect(bad.status).toBe(422)
    expect((bad.body as { field: string }).field).toBe('drive_letter')
    const good = await body<{ settings: { bandwidth: { up_bps: number; down_bps: number } } }>(agent, 'PATCH', '/v1/settings', { bandwidth: { up_bps: 1000 } })
    expect(good.settings.bandwidth).toEqual({ up_bps: 1000, down_bps: 0 })
  })
})
