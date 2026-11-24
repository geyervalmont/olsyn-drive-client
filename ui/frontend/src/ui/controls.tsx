import { useId, type ReactNode } from 'react'
import { Icon, type IconName } from './Icon'
import { Spinner } from './Progress'

export function Switch({
  checked,
  onChange,
  label,
  description,
  disabled,
}: {
  checked: boolean
  onChange: (next: boolean) => void
  label: string
  description?: string
  disabled?: boolean
}) {
  const id = useId()
  return (
    <div className={'switch-row' + (disabled ? ' is-disabled' : '')}>
      <button
        type="button"
        role="switch"
        className="switch"
        aria-checked={checked}
        aria-labelledby={id + '-l'}
        aria-describedby={description ? id + '-d' : undefined}
        disabled={disabled}
        onClick={() => onChange(!checked)}
      >
        <span className="switch-knob" />
      </button>
      <span className="switch-text">
        <span id={id + '-l'} className="switch-label" onClick={() => !disabled && onChange(!checked)}>
          {label}
        </span>
        {description && (
          <span id={id + '-d'} className="switch-desc">
            {description}
          </span>
        )}
      </span>
    </div>
  )
}

export function Button({
  children,
  onClick,
  variant = 'secondary',
  size = 'm',
  icon,
  pending,
  disabled,
  type = 'button',
  autoFocus,
  title,
  ...rest
}: {
  children: ReactNode
  onClick?: () => void
  variant?: 'primary' | 'secondary' | 'ghost' | 'danger' | 'danger-ghost'
  size?: 's' | 'm' | 'l'
  icon?: IconName
  pending?: boolean
  disabled?: boolean
  type?: 'button' | 'submit'
  autoFocus?: boolean
  title?: string
  'aria-describedby'?: string
  'data-autofocus'?: boolean
}) {
  return (
    <button
      type={type}
      className={'btn btn-' + size + ' btn-' + variant + (pending ? ' is-pending' : '')}
      onClick={onClick}
      disabled={disabled || pending}
      aria-busy={pending || undefined}
      autoFocus={autoFocus}
      title={title}
      {...rest}
    >
      {pending ? <Spinner size={size === 's' ? 13 : 15} /> : icon ? <Icon name={icon} size={size === 's' ? 15 : 17} /> : null}
      <span>{children}</span>
    </button>
  )
}

export function IconButton({ icon, label, onClick, disabled, pressed }: { icon: IconName; label: string; onClick: () => void; disabled?: boolean; pressed?: boolean }) {
  return (
    <button type="button" className={'icon-btn' + (pressed ? ' is-pressed' : '')} aria-label={label} title={label} onClick={onClick} disabled={disabled} aria-pressed={pressed}>
      <Icon name={icon} />
    </button>
  )
}

export function Field({ label, hint, error, children, htmlFor }: { label: string; hint?: ReactNode; error?: string | null; children: ReactNode; htmlFor: string }) {
  return (
    <div className={'field' + (error ? ' has-error' : '')}>
      <label className="field-label" htmlFor={htmlFor}>
        {label}
      </label>
      {children}
      {hint && !error && <p className="field-hint">{hint}</p>}
      {error && (
        <p className="field-error" role="alert">
          {error}
        </p>
      )}
    </div>
  )
}

export function EmptyState({ icon, title, children, action }: { icon: IconName; title: string; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="empty">
      <span className="empty-icon">
        <Icon name={icon} size={22} />
      </span>
      <h3 className="empty-title display">{title}</h3>
      {children && <p className="empty-body">{children}</p>}
      {action && <div className="empty-action">{action}</div>}
    </div>
  )
}

export function InlineError({ message, onRetry }: { message: string; onRetry?: () => void }) {
  return (
    <div className="inline-error" role="alert">
      <Icon name="alert" size={16} />
      <span>{message}</span>
      {onRetry && (
        <button type="button" className="link-btn" onClick={onRetry}>
          Try again
        </button>
      )}
    </div>
  )
}
