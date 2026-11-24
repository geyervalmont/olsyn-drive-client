// The states the mock agent can start in. Pick one with ?scenario=<name>.

export const scenarios = {
  copy: 'A copy of 3,000 small files and a 3 GB video going up, with realistic speed and time left.',
  'first-run': 'A new PC: welcome, sign in with a code, the driver step, pick a letter, done.',
  signin: 'Signed out on a PC that already has the driver: sign in and pick a letter.',
  'letter-taken': 'A new PC where W: is a remembered network drive: sign in, and the letter step picks X: and says why.',
  'link-expired': 'Sign-in where the code runs out before the browser approves it.',
  restart: 'A copy in progress when the drive restarts: uploads wait, then carry on.',
  offline: 'The drive cannot be reached; saved files wait on this PC.',
  held: 'Three files the drive refused: a managed folder, no permission, a name not allowed.',
  conflict: 'A file changed on the drive while this PC was saving it: a conflict copy was made.',
  update: 'An update is available; install it and watch it apply.',
  'driver-missing': 'Signed in, but the file system driver is missing.',
  'signed-out': 'Signed out by the drive (this PC was removed in the web app).',
  'big-list': '12,000 files waiting while paused, to check the list stays smooth.',
  empty: 'Connected with nothing moving and no history.',
  loading: 'The agent takes a few seconds to answer.',
  unreachable: 'The agent is not running at first, then starts.',
} as const

export type ScenarioName = keyof typeof scenarios

export function isScenario(name: string | null | undefined): name is ScenarioName {
  return !!name && Object.prototype.hasOwnProperty.call(scenarios, name)
}

export const scenarioNames = Object.keys(scenarios) as ScenarioName[]
