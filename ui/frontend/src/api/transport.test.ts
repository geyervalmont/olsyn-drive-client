import { afterEach, describe, expect, it, vi } from 'vitest'
import { BridgeTransport, installBridgeReceiver } from './bridge'
import { AgentUnreachableError } from './errors'
import { HttpTransport } from './transport'

type Call = { url: string; init?: RequestInit }

// A fetch that behaves like a browser's: called with any `this` other than
// the global object (or undefined) it rejects with "Illegal invocation", as
// WebView2 did for every /agent request on the first Windows run.
function browserLikeFetch(handler: (url: string, init?: RequestInit) => Response | Promise<Response>) {
  const calls: Call[] = []
  const fetchLike = function (this: unknown, input: RequestInfo | URL, init?: RequestInit) {
    if (this !== undefined && this !== globalThis) {
      return Promise.reject(new TypeError("Failed to execute 'fetch' on 'Window': Illegal invocation"))
    }
    calls.push({ url: String(input), init })
    return Promise.resolve(handler(String(input), init))
  }
  return { fetch: fetchLike as typeof fetch, calls }
}

const json = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })

afterEach(() => {
  vi.unstubAllGlobals()
})

describe('HttpTransport', () => {
  it('calls fetch with the global object as this', async () => {
    const { fetch, calls } = browserLikeFetch(() => json({ state: 'waiting' }, 202))
    const t = new HttpTransport({ baseUrl: '/agent', fetch })
    expect(await t.request('POST', '/v1/signin', { open_browser: true })).toEqual({ status: 202, body: { state: 'waiting' } })
    expect(calls).toHaveLength(1)
    expect(calls[0].url).toBe('/agent/v1/signin')
    expect(calls[0].init?.method).toBe('POST')
    expect(calls[0].init?.body).toBe('{"open_browser":true}')
  })

  it('keeps why a request failed', async () => {
    const t = new HttpTransport({ baseUrl: '/agent', fetch: (() => Promise.reject(new TypeError('Failed to fetch'))) as typeof fetch })
    const err = await t.request('GET', '/v1/state').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(AgentUnreachableError)
    expect((err as Error).cause).toBeInstanceOf(TypeError)
  })
})

describe('BridgeTransport', () => {
  it('reaches the agent with the window\'s own fetch (no fetch passed in)', async () => {
    const { fetch, calls } = browserLikeFetch((url) => (url === '/agent/v1/signin' ? json({ state: 'waiting' }, 202) : json({})))
    vi.stubGlobal('fetch', fetch)
    const t = new BridgeTransport('main', installBridgeReceiver({} as Window))
    const r = await t.request('POST', '/v1/signin', { open_browser: true })
    expect(r.status).toBe(202)
    expect(calls.map((c) => c.url)).toEqual(['/agent/v1/signin'])
  })

  it('tells the shell about a request that failed inside the page', async () => {
    const calls: Call[] = []
    const fetchLike = ((input: RequestInfo | URL, init?: RequestInit) => {
      calls.push({ url: String(input), init })
      if (String(input).startsWith('/agent/')) return Promise.reject(new TypeError('Failed to fetch'))
      return Promise.resolve(new Response(null, { status: 204 }))
    }) as typeof fetch
    const t = new BridgeTransport('main', installBridgeReceiver({} as Window), fetchLike)
    await expect(t.request('POST', '/v1/transfers/t_1/retry?x=1', {})).rejects.toBeInstanceOf(AgentUnreachableError)
    await vi.waitFor(() => expect(calls.some((c) => c.url === '/bridge/log')).toBe(true))
    const log = calls.find((c) => c.url === '/bridge/log')!
    expect(JSON.parse(String(log.init?.body))).toEqual({ method: 'POST', path: '/v1/transfers/t_1/retry', error: 'TypeError: Failed to fetch' })
  })
})
