// The desktop shell's side of the transport. The shell (ui/, Go + Wails) owns the
// connection to the agent's pipe or socket:
//   - requests: same-origin fetch to /agent/v1/..., proxied by the shell as is
//   - events:   POST /bridge/stream asks the shell to open GET /v1/events for this
//               window; the shell pushes each event in by calling
//               window.__olsynBridge.receive(message)
//   - shell actions (folder picker, windows, drops): /bridge/* and pushed messages

import { AgentBlockedError, AgentUnreachableError, type BlockedInfo, describeFailure } from './errors'
import type { SSEMessage } from './sse'
import { HttpTransport, type StreamHandlers, type Transport, type TransportResponse } from './transport'

export type BridgeMessage =
  | { kind: 'stream'; status: 'open' }
  | { kind: 'stream'; status: 'error'; message: string; blocked?: BlockedInfo }
  | { kind: 'events'; events: SSEMessage[] }
  | { kind: 'drop'; paths: string[] }
  | { kind: 'route'; route: string }

type Listener = (message: BridgeMessage) => void

/** The global the shell calls into. Created once per page. */
export interface BridgeReceiver {
  receive(message: BridgeMessage): void
  listen(listener: Listener): () => void
}

declare global {
  interface Window {
    __olsynBridge?: BridgeReceiver
  }
}

export function installBridgeReceiver(target: Window = window): BridgeReceiver {
  if (target.__olsynBridge) return target.__olsynBridge
  const listeners = new Set<Listener>()
  const receiver: BridgeReceiver = {
    receive(message) {
      for (const listener of [...listeners]) listener(message)
    },
    listen(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
  target.__olsynBridge = receiver
  return receiver
}

export async function bridgeCall<T>(path: string, body: unknown, fetchImpl: typeof fetch = fetch): Promise<T> {
  let response: Response
  try {
    response = await fetchImpl.call(globalThis, '/bridge/' + path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
      body: JSON.stringify(body ?? {}),
    })
  } catch (err) {
    throw new AgentUnreachableError('The Olsyn Drive window lost touch with its app.', err)
  }
  const text = await response.text()
  if (!response.ok) throw new Error(text || 'The shell refused ' + path)
  return (text ? JSON.parse(text) : null) as T
}

export class BridgeTransport implements Transport {
  private readonly http: HttpTransport
  private readonly receiver: BridgeReceiver

  constructor(
    private readonly windowName: string,
    receiver?: BridgeReceiver,
    private readonly fetchImpl: typeof fetch = fetch,
  ) {
    this.http = new HttpTransport({ baseUrl: '/agent', fetch: fetchImpl })
    this.receiver = receiver ?? installBridgeReceiver()
  }

  async request(method: string, path: string, body?: unknown, signal?: AbortSignal): Promise<TransportResponse> {
    try {
      return await this.http.request(method, path, body, signal)
    } catch (err) {
      // A request that fails inside the page never reaches the shell, so
      // nothing would be logged: tell the shell (method, path and the error,
      // never a body) so it lands in ui.log.
      if (err instanceof AgentUnreachableError) this.report(method, path, err)
      throw err
    }
  }

  private report(method: string, path: string, err: unknown) {
    const body = { method, path: path.split('?')[0], error: describeFailure(err) }
    void bridgeCall('log', body, this.fetchImpl).catch(() => undefined)
  }

  stream(lastEventId: string | null, handlers: StreamHandlers): () => void {
    let closed = false
    const stop = this.receiver.listen((message) => {
      if (closed) return
      if (message.kind === 'stream') {
        if (message.status === 'open') handlers.onOpen()
        else {
          closed = true
          stop()
          handlers.onError(message.blocked ? new AgentBlockedError(message.blocked) : new AgentUnreachableError(message.message))
        }
      } else if (message.kind === 'events') {
        for (const event of message.events) handlers.onMessage(event)
      }
    })
    bridgeCall('stream', { window: this.windowName, last_event_id: lastEventId }, this.fetchImpl).catch((err: unknown) => {
      if (closed) return
      closed = true
      stop()
      handlers.onError(err instanceof Error ? err : new Error(String(err)))
    })
    return () => {
      if (closed) return
      closed = true
      stop()
      void bridgeCall('stream/close', { window: this.windowName }, this.fetchImpl).catch(() => undefined)
    }
  }
}
