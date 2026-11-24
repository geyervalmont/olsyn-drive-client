// Open-source components shipped in the desktop app, for the About screen.
// The lists come from licences.generated.ts, written by internal/notices/gen
// from go.mod, ui/go.mod and package.json; the full texts come from the agent
// (GET /v1/licences) and ship as NOTICE.txt.

export interface Licence {
  name: string
  version: string
  licence: string
  url: string
  note?: string
}

export { frontEndLicences, goLicences, winfspNotice, winfspRepo } from './licences.generated'
