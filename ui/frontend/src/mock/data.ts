// Synthetic names and folders for the mock agent. Nothing here is real data.

const BS = String.fromCharCode(92)

export function winPath(letter: string, drivePath: string): string {
  return letter + drivePath.split('/').join(BS)
}

export function localPath(...parts: string[]): string {
  return parts.join(BS)
}

/** Deterministic pseudo-random numbers (mulberry32) so tests and screenshots repeat. */
export function seededRandom(seed: number): () => number {
  let a = seed >>> 0
  return () => {
    a = (a + 0x6d2b79f5) >>> 0
    let t = a
    t = Math.imul(t ^ (t >>> 15), t | 1)
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

const folders = [
  '/Projects/Harbour Tower/Site photos/2026-11-20',
  '/Projects/Harbour Tower/Drawings/Issued',
  '/Projects/Harbour Tower/Renders/Lobby',
  '/Projects/Riverside Library/Material samples',
  '/Projects/Riverside Library/Consultant markups',
  '/Studio/Moodboards/Warm minimal',
]

const stems = [
  'IMG_', 'DSC_', 'Facade study ', 'Lobby render v', 'Plan markup ', 'Sample board ', 'Detail section ',
  'Elevation north ', 'Timber texture ', 'Stair option ',
]
const exts = ['.jpg', '.jpg', '.jpg', '.png', '.pdf', '.tif', '.dwg', '.png']

export interface FileSpec {
  name: string
  drivePath: string
  size: number
  skip: 'already_there' | 'junk' | null
}

/** n small files, 8 KB to ~6 MB, a few already on the drive and a little OS junk. */
export function smallFiles(n: number, random: () => number): FileSpec[] {
  const out: FileSpec[] = []
  for (let i = 0; i < n; i++) {
    const folder = folders[Math.floor(random() * folders.length)]
    const junk = i > 0 && i % 997 === 0
    const stem = stems[Math.floor(random() * stems.length)]
    const name = junk ? 'Thumbs.db' : stem + String(1000 + i) + exts[Math.floor(random() * exts.length)]
    // Log-normal-ish: most files are a few hundred KB, a few are several MB.
    const size = Math.round(8 * 1024 + Math.exp(random() * 6.5 + random() * 3) * 1024)
    out.push({
      name,
      drivePath: folder + '/' + name,
      size: Math.min(size, 6 * 1024 * 1024),
      skip: junk ? 'junk' : random() < 0.03 ? 'already_there' : null,
    })
  }
  return out
}

export const bigFile: FileSpec = {
  name: 'Site flythrough 4K master.mov',
  drivePath: '/Projects/Harbour Tower/Renders/Site flythrough 4K master.mov',
  size: 3 * 1024 * 1024 * 1024,
  skip: null,
}

/** A drive folder tree for the destination picker (GET /v1/drive/folders). */
export const driveTree: Record<string, string[]> = {
  '/': ['Projects', 'Studio', 'Shared with me'],
  '/Projects': ['Harbour Tower', 'Riverside Library', 'Archive (read only)'],
  '/Projects/Harbour Tower': ['Drawings', 'Renders', 'Site photos', 'Client issue (managed)'],
  '/Projects/Harbour Tower/Drawings': ['Issued', 'Working'],
  '/Projects/Harbour Tower/Renders': ['Lobby', 'Exterior'],
  '/Projects/Harbour Tower/Site photos': ['2026-11-20'],
  '/Projects/Riverside Library': ['Material samples', 'Consultant markups'],
  '/Studio': ['Moodboards', 'Templates'],
  '/Shared with me': [],
}

export const readOnlyFolders = new Set(['/Projects/Archive (read only)', '/Projects/Harbour Tower/Client issue (managed)', '/Shared with me'])
