import { render } from '@testing-library/react'
import type { ReactNode } from 'react'
import { AgentClient } from '../api/client'
import type { Shell } from '../lib/shell'
import { MockAgent } from '../mock/agent'
import type { ScenarioName } from '../mock/scenarios'
import { MockTransport } from '../mock/transport'
import { AgentProvider } from '../state/context'
import { AgentConnection } from '../state/connection'
import { createStore } from '../state/store'
import { ToastProvider } from '../ui/Toasts'

export interface TestShell extends Shell {
  picked: string[]
  opened: string[]
}

export function testShell(folder: string | null = 'C:\\Users\\ada\\Documents'): TestShell {
  const picked: string[] = []
  const opened: string[] = []
  return {
    kind: 'desktop',
    picked,
    opened,
    async pickFolder(title) {
      picked.push(title)
      return folder
    },
    showMain: () => undefined,
    hidePopover: () => undefined,
    quitApp: () => undefined,
    openUrl: (url) => {
      opened.push(url)
    },
    onDrop: () => () => undefined,
    onRoute: () => () => undefined,
  }
}

export function renderWithAgent(ui: ReactNode, scenario: ScenarioName, shell: TestShell = testShell()) {
  const agent = new MockAgent({ scenario })
  const client = new AgentClient(new MockTransport(agent, 0))
  const connection = new AgentConnection(client, createStore())
  const result = render(
    <AgentProvider value={{ client, connection, shell }}>
      <ToastProvider>{ui}</ToastProvider>
    </AgentProvider>,
  )
  return { ...result, agent, client, connection, shell }
}
