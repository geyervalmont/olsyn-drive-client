// What the UI needs from the desktop shell beyond the agent's API: native folder
// pickers, window management, dropped paths and routes from olsyn-drive:// links.
// In a browser (mock mode) a stand-in keeps every screen usable.

import { bridgeCall, installBridgeReceiver } from '../api/bridge'

export interface Shell {
  readonly kind: 'desktop' | 'browser'
  /** Asks the person for a folder on this PC. null when they cancel. */
  pickFolder(title: string): Promise<string | null>
  /** Opens (or focuses) the main window, optionally at a route such as "transfers/held". */
  showMain(route?: string): void
  /** Hides the tray popover (desktop only). */
  hidePopover(): void
  /** Closes the app's windows and tray icon; the agent and the drive keep running. */
  quitApp(): void
  /** Opens a web page in the person's browser. */
  openUrl(url: string): void
  onDrop(listener: (paths: string[]) => void): () => void
  onRoute(listener: (route: string) => void): () => void
}

export function createDesktopShell(windowName: string): Shell {
  const receiver = installBridgeReceiver()
  const windowAction = (action: string, route?: string) => {
    void bridgeCall('window', { window: windowName, action, route }).catch(() => undefined)
  }
  // The Wails runtime turns OS file drops on [data-file-drop-target] into paths for the shell.
  const runtimeUrl = ['', 'wails', 'runtime.js'].join('/')
  void import(/* @vite-ignore */ runtimeUrl).catch(() => undefined)
  return {
    kind: 'desktop',
    async pickFolder(title) {
      const result = await bridgeCall<{ path: string | null }>('pick-folder', { title })
      return result?.path ?? null
    },
    showMain: (route) => windowAction('show-main', route),
    hidePopover: () => windowAction('hide-popover'),
    quitApp: () => windowAction('quit-ui'),
    openUrl: (url) => {
      void bridgeCall('open-url', { url }).catch(() => undefined)
    },
    onDrop(listener) {
      return receiver.listen((m) => {
        if (m.kind === 'drop') listener(m.paths)
      })
    },
    onRoute(listener) {
      return receiver.listen((m) => {
        if (m.kind === 'route') listener(m.route)
      })
    },
  }
}

/** Browser stand-in used with the mock agent. */
export function createBrowserShell(): Shell {
  const routeListeners = new Set<(route: string) => void>()
  const dropListeners = new Set<(paths: string[]) => void>()
  // Dropping files in a browser gives names, not paths: make a plausible path for the mock.
  if (typeof window !== 'undefined') {
    window.addEventListener('dragover', (e) => e.preventDefault())
    window.addEventListener('drop', (e) => {
      const target = e.target instanceof Element ? e.target.closest('[data-file-drop-target]') : null
      if (!target) return
      e.preventDefault()
      const files = Array.from(e.dataTransfer?.files ?? [])
      const paths = files.length ? files.map((f) => 'C:\\Users\\you\\Desktop\\' + f.name) : ['C:\\Users\\you\\Desktop\\Dropped folder']
      for (const l of dropListeners) l(paths)
    })
  }
  return {
    kind: 'browser',
    async pickFolder() {
      return 'C:\\Users\\you\\Documents'
    },
    showMain(route) {
      if (route) for (const l of routeListeners) l(route)
    },
    hidePopover: () => undefined,
    quitApp: () => undefined,
    openUrl: (url) => {
      window.open(url, '_blank', 'noopener,noreferrer')
    },
    onDrop(listener) {
      dropListeners.add(listener)
      return () => dropListeners.delete(listener)
    },
    onRoute(listener) {
      routeListeners.add(listener)
      return () => routeListeners.delete(listener)
    },
  }
}
