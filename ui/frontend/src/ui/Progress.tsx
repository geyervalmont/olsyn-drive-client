export function ProgressBar({
  value,
  label,
  tone = 'busy',
  size = 'm',
  valueText,
}: {
  /** 0..1, or null for "working, amount unknown". */
  value: number | null
  label: string
  tone?: 'busy' | 'paused' | 'warn' | 'off' | 'ok'
  size?: 's' | 'm'
  valueText?: string
}) {
  const pct = value === null ? null : Math.round(Math.max(0, Math.min(1, value)) * 1000) / 10
  return (
    <div
      className={'progress progress-' + size + ' tone-' + tone + (pct === null ? ' is-indeterminate' : '')}
      role="progressbar"
      aria-label={label}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={pct === null ? undefined : Math.round(pct)}
      aria-valuetext={valueText}
    >
      <div className="progress-fill" style={pct === null ? undefined : { width: pct + '%' }} />
    </div>
  )
}

export function Spinner({ label, size = 16 }: { label?: string; size?: number }) {
  return (
    <span className="spinner" role={label ? 'status' : undefined} aria-label={label}>
      <svg width={size} height={size} viewBox="0 0 24 24" aria-hidden="true" focusable="false">
        <circle cx="12" cy="12" r="9" fill="none" stroke="currentColor" strokeOpacity="0.2" strokeWidth="2.4" />
        <path d="M21 12a9 9 0 0 0-9-9" fill="none" stroke="currentColor" strokeWidth="2.4" strokeLinecap="round" />
      </svg>
    </span>
  )
}
