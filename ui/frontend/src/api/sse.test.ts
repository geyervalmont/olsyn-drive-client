import { describe, expect, it } from 'vitest'
import { createSSEParser, type SSEMessage } from './sse'

function parse(chunks: string[]) {
  const out: SSEMessage[] = []
  const comments: string[] = []
  const p = createSSEParser((m) => out.push(m), (c) => comments.push(c))
  for (const c of chunks) p.push(c)
  return { out, comments, last: p.lastEventId }
}

describe('SSE parser', () => {
  it('parses events with ids, names and data', () => {
    const { out, last } = parse(['id: r1-1\nevent: state\ndata: {"a":1}\n\n', 'id: r1-2\nevent: summary\ndata: {"b":2}\n\n'])
    expect(out).toEqual([
      { id: 'r1-1', event: 'state', data: '{"a":1}' },
      { id: 'r1-2', event: 'summary', data: '{"b":2}' },
    ])
    expect(last).toBe('r1-2')
  })

  it('handles chunks split anywhere, CRLF and multi-line data', () => {
    const text = 'id: x-9\r\nevent: transfer\r\ndata: line one\r\ndata: line two\r\n\r\n'
    const chunks = text.split('')
    const { out } = parse(chunks)
    expect(out).toEqual([{ id: 'x-9', event: 'transfer', data: 'line one\nline two' }])
  })

  it('reports comments (pings) and ignores retry and unknown fields', () => {
    const { out, comments } = parse([': ping\n\nretry: 10\nfoo: bar\ndata: x\n\n'])
    expect(comments).toEqual(['ping'])
    expect(out).toEqual([{ id: null, event: 'message', data: 'x' }])
  })

  it('keeps the last id when an event has none', () => {
    const { out } = parse(['id: a-1\ndata: 1\n\n', 'data: 2\n\n'])
    expect(out.map((m) => m.id)).toEqual(['a-1', 'a-1'])
  })

  it('does not dispatch a block without data', () => {
    const { out, last } = parse(['id: a-5\nevent: state\n\n'])
    expect(out).toEqual([])
    expect(last).toBe('a-5')
  })
})
