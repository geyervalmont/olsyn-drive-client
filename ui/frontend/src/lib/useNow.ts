import { useEffect, useState } from 'react'

/** The current time, refreshed every intervalMs while enabled. */
export function useNow(intervalMs = 1000, enabled = true): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    if (!enabled) return
    const id = setInterval(() => setNow(Date.now()), intervalMs)
    return () => clearInterval(id)
  }, [intervalMs, enabled])
  return now
}

/** Smooth progress between transfer events: bytes_done plus what bps predicts since the event, never past size. */
export function interpolatedBytes(t: { bytes_done: number; bps: number; size: number; state: string; receivedAt?: number }, now: number): number {
  if (t.state !== 'active' || !t.receivedAt || t.bps <= 0) return Math.min(t.bytes_done, t.size)
  const elapsed = Math.min(1.5, Math.max(0, (now - t.receivedAt) / 1000))
  return Math.min(t.size, t.bytes_done + t.bps * elapsed)
}
