// The mock agent behind the Transport interface, with a little latency so
// pending states are visible.

import { AgentUnreachableError } from '../api/errors'
import type { StreamHandlers, Transport, TransportResponse } from '../api/transport'
import type { MockAgent } from './agent'

export class MockTransport implements Transport {
  constructor(
    readonly agent: MockAgent,
    private readonly latencyMs = 120,
  ) {}

  async request(method: string, path: string, body?: unknown): Promise<TransportResponse> {
    if (this.latencyMs > 0) await new Promise((r) => setTimeout(r, this.latencyMs * (0.5 + Math.random())))
    try {
      // Round-trip through JSON so the UI never shares objects with the mock.
      const response = await this.agent.handle(method, path, body === undefined ? undefined : JSON.parse(JSON.stringify(body)))
      return { status: response.status, body: response.body === null ? null : JSON.parse(JSON.stringify(response.body)) }
    } catch {
      throw new AgentUnreachableError()
    }
  }

  stream(lastEventId: string | null, handlers: StreamHandlers): () => void {
    return this.agent.subscribe(lastEventId, {
      onOpen: () => handlers.onOpen(),
      onMessage: (m) => handlers.onMessage(m),
      onError: (e) => handlers.onError(e),
    })
  }
}
