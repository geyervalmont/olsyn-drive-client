import { fireEvent, render, screen } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { VirtualList } from './VirtualList'

describe('VirtualList', () => {
  beforeEach(() => {
    // jsdom has no layout: give the scroll box a size.
    vi.spyOn(HTMLElement.prototype, 'getBoundingClientRect').mockReturnValue({ x: 0, y: 0, top: 0, left: 0, bottom: 600, right: 800, width: 800, height: 600, toJSON: () => ({}) })
    vi.spyOn(HTMLElement.prototype, 'clientHeight', 'get').mockReturnValue(600)
    vi.spyOn(HTMLElement.prototype, 'clientWidth', 'get').mockReturnValue(800)
    vi.spyOn(HTMLElement.prototype, 'offsetHeight', 'get').mockReturnValue(600)
    vi.spyOn(HTMLElement.prototype, 'offsetWidth', 'get').mockReturnValue(800)
  })
  afterEach(() => vi.restoreAllMocks())

  it('renders only the rows on screen out of 12,000', async () => {
    const items = Array.from({ length: 12_000 }, (_, i) => ({ id: 't' + i, name: 'File ' + i }))
    const start = performance.now()
    render(<VirtualList items={items} rowHeight={60} getKey={(x) => x.id} label="Waiting" renderRow={(x) => <span>{x.name}</span>} />)
    const took = performance.now() - start
    const rows = await screen.findAllByRole('listitem')
    expect(rows.length).toBeGreaterThan(5)
    expect(rows.length).toBeLessThan(40)
    expect(rows[0].getAttribute('aria-setsize')).toBe('12000')
    expect(rows[0].getAttribute('aria-posinset')).toBe('1')
    expect(took).toBeLessThan(1000)
  })

  it('renders short lists plainly and moves focus with the arrow keys', async () => {
    const items = Array.from({ length: 5 }, (_, i) => ({ id: 'r' + i }))
    render(<VirtualList items={items} rowHeight={60} getKey={(x) => x.id} label="Held files" renderRow={(x) => <span>{x.id}</span>} />)
    const rows = screen.getAllByRole('listitem')
    expect(rows).toHaveLength(5)
    rows[0].focus()
    fireEvent.keyDown(rows[0], { key: 'ArrowDown' })
    await new Promise((r) => requestAnimationFrame(() => r(null)))
    expect(document.activeElement).toBe(rows[1])
    fireEvent.keyDown(rows[1], { key: 'End' })
    await new Promise((r) => requestAnimationFrame(() => r(null)))
    expect(document.activeElement).toBe(rows[4])
  })
})
