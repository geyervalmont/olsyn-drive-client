// Mock mode only: switch scenario and surface without editing the URL.

import { scenarioNames, scenarios, type ScenarioName } from './mock/scenarios'

export function DevBar({ scenario, surface }: { scenario: ScenarioName; surface: string }) {
  const go = (params: Record<string, string>) => {
    const q = new URLSearchParams(location.search)
    for (const [k, v] of Object.entries(params)) q.set(k, v)
    location.search = q.toString()
  }
  return (
    <aside className="devbar" aria-label="Mock controls">
      <span className="devbar-tag">Mock</span>
      <label>
        <span className="sr-only">Scenario</span>
        <select value={scenario} onChange={(e) => go({ scenario: e.target.value })} title={scenarios[scenario]}>
          {scenarioNames.map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </label>
      <label>
        <span className="sr-only">Surface</span>
        <select value={surface} onChange={(e) => go({ view: e.target.value })}>
          <option value="main">Window</option>
          <option value="popover">Tray popover</option>
        </select>
      </label>
    </aside>
  )
}
