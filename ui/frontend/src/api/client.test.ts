import { describe, expect, it, vi } from 'vitest'
import { AgentClient, parseEvent } from './client'
import { AgentError, AgentUnreachableError } from './errors'
import { HttpTransport, type StreamHandlers, type Transport, type TransportResponse } from './transport'

class FakeTransport implements Transport {
  calls: { method: string; path: string; body: unknown }[] = []
  constructor(private readonly reply: (method: string, path: string, body: unknown) => TransportResponse) {}
  async request(method: string, path: string, body?: unknown) {
    this.calls.push({ method, path, body })
    return this.reply(method, path, body)
  }
  handlers: StreamHandlers | null = null
  lastId: string | null = null
  stream(lastEventId: string | null, handlers: StreamHandlers) {
    this.lastId = lastEventId
    this.handlers = handlers
    return () => {
      this.handlers = null
    }
  }
}

describe('AgentClient', () => {
  it('sends the documented methods, paths and bodies', async () => {
    const t = new FakeTransport(() => ({ status: 200, body: {} }))
    const c = new AgentClient(t)
    await c.signIn()
    await c.signOut({ clearCache: true })
    await c.transfers({ state: ['done', 'skipped'], cursor: 'c_1', limit: 9999 })
    await c.saveHeldCopy('t 1', 'C:\\Docs')
    await c.discardHeld('t_2')
    await c.pause(60)
    await c.pause()
    await c.patchSettings({ bandwidth: { up_bps: 5 } })
    await c.applyUpdate('now')
    await c.send(['D:\\Exports', 'D:\\Plans.pdf'], '/Projects')
    await c.driveFolders('/Projects')
    await c.activity('a_50', 20)
    await c.quit(true)
    expect(t.calls).toEqual([
      { method: 'POST', path: '/v1/signin', body: { open_browser: true } },
      { method: 'POST', path: '/v1/signout', body: { clear_cache: true, discard_unsent_confirmed: false } },
      { method: 'GET', path: '/v1/transfers?state=done%2Cskipped&cursor=c_1&limit=500', body: undefined },
      { method: 'POST', path: '/v1/held/t%201/save-copy', body: { dest_dir: 'C:\\Docs' } },
      { method: 'POST', path: '/v1/held/t_2/discard', body: {} },
      { method: 'POST', path: '/v1/pause', body: { minutes: 60 } },
      { method: 'POST', path: '/v1/pause', body: {} },
      { method: 'PATCH', path: '/v1/settings', body: { bandwidth: { up_bps: 5 } } },
      { method: 'POST', path: '/v1/update/apply', body: { when: 'now' } },
      { method: 'POST', path: '/v1/send', body: { local_paths: ['D:\\Exports', 'D:\\Plans.pdf'], drive_folder: '/Projects' } },
      { method: 'GET', path: '/v1/drive/folders?parent=%2FProjects', body: undefined },
      { method: 'GET', path: '/v1/activity?limit=20&cursor=a_50', body: undefined },
      { method: 'POST', path: '/v1/quit', body: { unmount: true } },
    ])
  })

  it('turns error bodies into AgentError with code, message and detail', async () => {
    const t = new FakeTransport(() => ({
      status: 409,
      body: { error: 'unsent_data', message: '3 files have not reached the drive yet.', unsent: { files: 3, bytes: 12 } },
    }))
    const err = await new AgentClient(t).signOut({ clearCache: true }).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(AgentError)
    expect((err as AgentError).code).toBe('unsent_data')
    expect((err as AgentError).status).toBe(409)
    expect((err as AgentError).message).toBe('3 files have not reached the drive yet.')
    expect((err as AgentError).detail).toEqual({ unsent: { files: 3, bytes: 12 } })
  })

  it('gives a readable message when the body is not the error shape', async () => {
    const t = new FakeTransport(() => ({ status: 503, body: null }))
    const err = (await new AgentClient(t).state().catch((e: unknown) => e)) as AgentError
    expect(err.code).toBe('internal')
    expect(err.message).toMatch(/can't be reached/)
  })

  it('treats 404 on proposed endpoints as "not supported"', async () => {
    const t = new FakeTransport(() => ({ status: 404, body: { error: 'not_found', message: 'no' } }))
    const c = new AgentClient(t)
    expect(await c.cache()).toBeNull()
    expect(await c.driveFolders('/')).toBeNull()
    expect(await c.activity()).toBeNull()
  })

  it('parses known events and drops unknown ones or bad JSON', () => {
    const events: unknown[] = []
    const t = new FakeTransport(() => ({ status: 200, body: {} }))
    const close = new AgentClient(t).subscribe('r-3', { onOpen: () => undefined, onEvent: (e) => events.push(e), onError: () => undefined })
    expect(t.lastId).toBe('r-3')
    t.handlers!.onMessage({ id: 'r-4', event: 'summary', data: '{"active":1}' })
    t.handlers!.onMessage({ id: 'r-5', event: 'future_thing', data: '{}' })
    t.handlers!.onMessage({ id: 'r-6', event: 'state', data: 'not json' })
    expect(events).toEqual([{ id: 'r-4', event: 'summary', data: { active: 1 } }])
    close()
    expect(parseEvent({ id: null, event: 'held', data: '5' })).toBeNull()
  })
})

describe('HttpTransport', () => {
  const streamOf = (chunks: string[]) =>
    new ReadableStream<Uint8Array>({
      start(controller) {
        const enc = new TextEncoder()
        for (const c of chunks) controller.enqueue(enc.encode(c))
        controller.close()
      },
    })

  it('posts JSON and reads JSON or 204', async () => {
    const fetchMock = vi.fn(async (url: string, init?: RequestInit) => {
      if (url.endsWith('/v1/open')) return new Response(null, { status: 204 })
      return new Response(JSON.stringify({ echo: init?.body ? JSON.parse(String(init.body)) : null }), { status: 200 })
    })
    const t = new HttpTransport({ baseUrl: '/agent/', fetch: fetchMock as unknown as typeof fetch })
    expect(await t.request('POST', '/v1/pause', { minutes: 5 })).toEqual({ status: 200, body: { echo: { minutes: 5 } } })
    expect(await t.request('POST', '/v1/open', { target: 'logs' })).toEqual({ status: 204, body: null })
    expect(fetchMock.mock.calls[0][0]).toBe('/agent/v1/pause')
    expect((fetchMock.mock.calls[0][1] as RequestInit).headers).toMatchObject({ 'Content-Type': 'application/json' })
  })

  it('reports a network failure as unreachable', async () => {
    const t = new HttpTransport({ baseUrl: '', fetch: (async () => Promise.reject(new TypeError('Failed to fetch'))) as unknown as typeof fetch })
    await expect(t.request('GET', '/v1/state')).rejects.toBeInstanceOf(AgentUnreachableError)
  })

  it('streams SSE, sends Last-Event-ID and reports the end as an error', async () => {
    let headers: Record<string, string> = {}
    const fetchMock = async (_url: string, init?: RequestInit) => {
      headers = init?.headers as Record<string, string>
      return new Response(streamOf(['id: r-1\nevent: sum', 'mary\ndata: {"active":2}\n\n: ping\n\n']), { status: 200 })
    }
    const t = new HttpTransport({ baseUrl: '', fetch: fetchMock as unknown as typeof fetch })
    const got: string[] = []
    const ended = new Promise<Error>((resolve) => {
      t.stream('r-0', { onOpen: () => got.push('open'), onMessage: (m) => got.push(m.event + ':' + m.data), onError: resolve })
    })
    const err = await ended
    expect(headers['Last-Event-ID']).toBe('r-0')
    expect(got).toEqual(['open', 'summary:{"active":2}'])
    expect(err.message).toMatch(/ended/)
  })
})
