import '@fontsource-variable/hanken-grotesk/wght.css'
import '@fontsource-variable/lora/wght.css'
import './styles/tokens.css'
import './styles/app.css'
import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { App, type Surface } from './App'
import { DevBar } from './DevBar'
import { AgentClient } from './api/client'
import { BridgeTransport } from './api/bridge'
import type { Transport } from './api/transport'
import { createBrowserShell, createDesktopShell, type Shell } from './lib/shell'
import { isScenario, type ScenarioName } from './mock/scenarios'
import { AgentProvider } from './state/context'
import { AgentConnection } from './state/connection'
import { createStore } from './state/store'
import { ToastProvider } from './ui/Toasts'

const params = new URLSearchParams(location.search)
const surface: Surface = params.get('view') === 'popover' ? 'popover' : 'main'
const useBridge = params.get('transport') === 'bridge'

let transport: Transport
let shell: Shell
let scenario: ScenarioName | null = null
if (useBridge) {
  transport = new BridgeTransport(surface)
  shell = createDesktopShell(surface)
} else {
  const name = params.get('scenario')
  scenario = isScenario(name) ? name : 'copy'
  const speed = Number(params.get('speed') || '1')
  // The mock is loaded only in a browser, so the desktop build does not carry its weight at start.
  const [{ MockAgent }, { MockTransport }] = await Promise.all([import('./mock/agent'), import('./mock/transport')])
  const agent = new MockAgent({ scenario, speed: Number.isFinite(speed) && speed > 0 ? speed : 1 })
  agent.start()
  transport = new MockTransport(agent)
  shell = createBrowserShell()
}

document.documentElement.dataset.surface = surface
const client = new AgentClient(transport)
const connection = new AgentConnection(client, createStore())

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <AgentProvider value={{ client, connection, shell }}>
      <ToastProvider>
        <App surface={surface} />
      </ToastProvider>
    </AgentProvider>
    {scenario && <DevBar scenario={scenario} surface={surface} />}
  </StrictMode>,
)
