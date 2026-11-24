// How the UI reaches the agent. The real agent listens on a named pipe or a unix
// socket that a webview cannot open, so the desktop shell proxies requests and
// pushes the event stream in; the mock transport answers in-process. Both sit
// behind this interface so the client, the state and the screens never know which.

import { AgentUnreachableError } from './errors'
import { createSSEParser, type SSEMessage } from './sse'

export interface TransportResponse {
  status: number
  body: unknown
}

export interface StreamHandlers {
  onOpen(): void
  onMessage(message: SSEMessage): void
  /** The stream ended or failed; the caller decides when to reconnect. */
  onError(error: Error): void
}

export interface Transport {
  request(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<TransportResponse>
  /** Opens GET /v1/events. Returns a function that closes the stream. */
  stream(lastEventId: string | null, handlers: StreamHandlers): () => void
}

/** The agent says nothing for this long: treat the stream as dead (docs/ipc.md: 45 s). */
export const STREAM_SILENCE_MS = 45_000

export interface HttpTransportOptions {
  /** Prefix for every path, e.g. "/agent" when the shell proxies under that path. */
  baseUrl: string
  fetch?: typeof fetch
  silenceMs?: number
}

/**
 * Plain HTTP: fetch for requests, a fetch stream parsed as SSE for events.
 * Same-origin only; the shell strips Origin before anything reaches the agent.
 */
export class HttpTransport implements Transport {
  private readonly base: string
  private readonly fetchImpl: typeof fetch
  private readonly silenceMs: number

  constructor(options: HttpTransportOptions) {
    this.base = options.baseUrl.replace(/\/$/, '')
    // fetch belongs to the global object. Called as a method of this class
    // (this.fetchImpl(...)) a browser rejects it with "Illegal invocation"
    // before anything is sent, so it is always called with globalThis as this.
    const f = options.fetch ?? globalThis.fetch
    this.fetchImpl = ((input: RequestInfo | URL, init?: RequestInit) => f.call(globalThis, input, init)) as typeof fetch
    this.silenceMs = options.silenceMs ?? STREAM_SILENCE_MS
  }

  async request(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<TransportResponse> {
    let response: Response
    try {
      response = await this.fetchImpl(this.base + path, {
        method,
        headers: body === undefined ? { Accept: 'application/json' } : { Accept: 'application/json', 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal,
      })
    } catch (err) {
      if (signal?.aborted) throw err
      throw new AgentUnreachableError(undefined, err)
    }
    if (response.status === 204) return { status: 204, body: null }
    const text = await response.text()
    let parsed: unknown = null
    if (text) {
      try {
        parsed = JSON.parse(text)
      } catch {
        parsed = null
      }
    }
    return { status: response.status, body: parsed }
  }

  stream(lastEventId: string | null, handlers: StreamHandlers): () => void {
    const controller = new AbortController()
    let closed = false
    let silence: ReturnType<typeof setTimeout> | undefined

    const armSilence = () => {
      if (silence) clearTimeout(silence)
      silence = setTimeout(() => {
        if (closed) return
        closed = true
        controller.abort()
        handlers.onError(new Error('The event stream went quiet.'))
      }, this.silenceMs)
    }

    const fail = (err: unknown) => {
      if (closed) return
      closed = true
      if (silence) clearTimeout(silence)
      handlers.onError(err instanceof Error ? err : new Error(String(err)))
    }

    const run = async () => {
      const headers: Record<string, string> = { Accept: 'text/event-stream' }
      if (lastEventId) headers['Last-Event-ID'] = lastEventId
      let response: Response
      try {
        response = await this.fetchImpl(this.base + '/v1/events', { headers, signal: controller.signal })
      } catch (err) {
        fail(new AgentUnreachableError(undefined, err))
        return
      }
      if (!response.ok || !response.body) {
        fail(new Error('The event stream was refused (' + response.status + ').'))
        return
      }
      handlers.onOpen()
      armSilence()
      const parser = createSSEParser(
        (m) => handlers.onMessage(m),
        () => undefined,
      )
      const reader = response.body.getReader()
      const decoder = new TextDecoder()
      try {
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          armSilence()
          parser.push(decoder.decode(value, { stream: true }))
        }
        fail(new Error('The event stream ended.'))
      } catch (err) {
        fail(err)
      }
    }
    void run()

    return () => {
      closed = true
      if (silence) clearTimeout(silence)
      controller.abort()
    }
  }
}
