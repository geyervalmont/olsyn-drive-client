// The drive letter set in Lora inside a ring. The ring fills with overall
// progress and takes the state's tone: the one image the app is known by.

import type { Tone } from '../state/derive'

export function LetterMark({
  letter,
  tone,
  progress,
  size = 44,
  label,
}: {
  letter: string
  tone: Tone
  progress: number | null
  size?: number
  label?: string
}) {
  const stroke = Math.max(2, Math.round(size / 18))
  const r = size / 2 - stroke
  const c = 2 * Math.PI * r
  const shown = progress === null ? (tone === 'ok' ? 1 : 0) : Math.max(0.02, Math.min(1, progress))
  return (
    <span className={'letter-mark tone-' + tone} style={{ width: size, height: size }} role={label ? 'img' : undefined} aria-label={label} aria-hidden={label ? undefined : true}>
      <svg width={size} height={size} viewBox={'0 0 ' + size + ' ' + size} aria-hidden="true" focusable="false">
        <circle className="letter-mark-track" cx={size / 2} cy={size / 2} r={r} strokeWidth={stroke} fill="none" />
        <circle
          className="letter-mark-ring"
          cx={size / 2}
          cy={size / 2}
          r={r}
          strokeWidth={stroke}
          fill="none"
          strokeDasharray={c}
          strokeDashoffset={c * (1 - shown)}
          transform={'rotate(-90 ' + size / 2 + ' ' + size / 2 + ')'}
        />
      </svg>
      <span className="letter-mark-glyph" style={{ fontSize: Math.round(size * 0.4) }}>
        {letter.replace(':', '')}
        <span className="letter-mark-colon">:</span>
      </span>
    </span>
  )
}
