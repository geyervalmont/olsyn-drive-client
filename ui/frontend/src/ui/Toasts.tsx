// In-window messages: action results, and in a browser (mock) the agent's
// notifications, which the desktop shell shows as Windows toasts instead.

import { createContext, useCallback, useContext, useMemo, useRef, useState, type ReactNode } from 'react'
import { Icon, type IconName } from './Icon'

export interface Toast {
  id: number
  title: string
  body?: string
  icon?: IconName
  action?: { label: string; run: () => void }
}

const Ctx = createContext<(toast: Omit<Toast, 'id'>) => void>(() => undefined)

export function useToast() {
  return useContext(Ctx)
}

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([])
  const next = useRef(1)
  const dismiss = useCallback((id: number) => setToasts((t) => t.filter((x) => x.id !== id)), [])
  const push = useCallback(
    (toast: Omit<Toast, 'id'>) => {
      const id = next.current++
      setToasts((t) => [...t.slice(-2), { ...toast, id }])
      setTimeout(() => dismiss(id), 6000)
    },
    [dismiss],
  )
  const value = useMemo(() => push, [push])
  return (
    <Ctx.Provider value={value}>
      {children}
      <div className="toasts" role="region" aria-label="Messages" aria-live="polite">
        {toasts.map((t) => (
          <div key={t.id} className="toast">
            {t.icon && (
              <span className="toast-icon">
                <Icon name={t.icon} size={17} />
              </span>
            )}
            <div className="toast-text">
              <p className="toast-title">{t.title}</p>
              {t.body && <p className="toast-body">{t.body}</p>}
            </div>
            {t.action && (
              <button
                type="button"
                className="link-btn"
                onClick={() => {
                  t.action!.run()
                  dismiss(t.id)
                }}
              >
                {t.action.label}
              </button>
            )}
            <button type="button" className="icon-btn toast-close" aria-label="Dismiss" onClick={() => dismiss(t.id)}>
              <Icon name="close" size={15} />
            </button>
          </div>
        ))}
      </div>
    </Ctx.Provider>
  )
}
