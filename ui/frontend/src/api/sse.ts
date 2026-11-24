// A small Server-Sent Events parser (WHATWG "event stream interpretation"),
// fed with text chunks as they arrive. Used over fetch streams and in tests.

export interface SSEMessage {
  id: string | null
  event: string
  data: string
}

export interface SSEParser {
  /** Feed a chunk of text; complete messages go to onMessage. */
  push(chunk: string): void
  /** The last id seen (for Last-Event-ID). */
  readonly lastEventId: string | null
}

export function createSSEParser(onMessage: (m: SSEMessage) => void, onComment?: (text: string) => void): SSEParser {
  let buffer = ''
  let eventName = ''
  let data: string[] = []
  let hasData = false
  let lastEventId: string | null = null
  let pendingId: string | null = null

  const dispatch = () => {
    if (pendingId !== null) lastEventId = pendingId
    if (hasData) {
      onMessage({ id: lastEventId, event: eventName || 'message', data: data.join('\n') })
    }
    eventName = ''
    data = []
    hasData = false
    pendingId = null
  }

  const line = (text: string) => {
    if (text === '') {
      dispatch()
      return
    }
    if (text.startsWith(':')) {
      onComment?.(text.slice(1).trimStart())
      return
    }
    const colon = text.indexOf(':')
    const field = colon === -1 ? text : text.slice(0, colon)
    let value = colon === -1 ? '' : text.slice(colon + 1)
    if (value.startsWith(' ')) value = value.slice(1)
    switch (field) {
      case 'event':
        eventName = value
        break
      case 'data':
        data.push(value)
        hasData = true
        break
      case 'id':
        if (!value.includes('\u0000')) pendingId = value
        break
      default:
        // "retry" and unknown fields are ignored: reconnect timing is ours.
        break
    }
  }

  return {
    push(chunk: string) {
      buffer += chunk
      for (;;) {
        const match = /\r\n|\r|\n/.exec(buffer)
        if (!match) break
        // A lone \r at the very end may be the first half of \r\n: wait for more.
        if (match[0] === '\r' && match.index === buffer.length - 1) break
        line(buffer.slice(0, match.index))
        buffer = buffer.slice(match.index + match[0].length)
      }
    },
    get lastEventId() {
      return lastEventId
    },
  }
}
