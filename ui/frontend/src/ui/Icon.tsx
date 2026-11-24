// A small hand-drawn icon set (24 px grid, 1.6 stroke). Decorative by default;
// pass label for an icon that carries meaning on its own.

import type { SVGProps } from 'react'

const paths = {
  check: 'M5 12.5l4.2 4.2L19 7',
  up: 'M12 19V5M6 11l6-6 6 6',
  down: 'M12 5v14M6 13l6 6 6-6',
  pause: 'M9 5.5v13M15 5.5v13',
  play: 'M8 5.5v13l10.5-6.5z',
  alert: 'M12 4.5l8.5 15h-17zM12 10v4.2M12 17.2v.1',
  offline: 'M3 3l18 18M8.5 8.6A5.5 5.5 0 0 0 6.6 18H17M11 6.6a6 6 0 0 1 7.4 4.9A4 4 0 0 1 21 17.6',
  refresh: 'M19.5 12a7.5 7.5 0 1 1-2.2-5.3M19.5 4.5v4.2h-4.2',
  folder: 'M3.5 7.5a1.5 1.5 0 0 1 1.5-1.5h4.4l2 2.2h7.6a1.5 1.5 0 0 1 1.5 1.5v8.3a1.5 1.5 0 0 1-1.5 1.5H5a1.5 1.5 0 0 1-1.5-1.5z',
  file: 'M7 3.5h6.5L18 8v12.5H7zM13.5 3.5V8H18',
  gear: 'M12 9.2a2.8 2.8 0 1 0 0 5.6 2.8 2.8 0 0 0 0-5.6zM12 3.5l1.4 2.3 2.6-.6.6 2.6 2.3 1.4-1.2 2.4 1.2 2.4-2.3 1.4-.6 2.6-2.6-.6L12 20.5l-1.4-2.3-2.6.6-.6-2.6-2.3-1.4 1.2-2.4-1.2-2.4 2.3-1.4.6-2.6 2.6.6z',
  user: 'M12 12a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM4.5 20c.8-3.6 3.8-5.5 7.5-5.5s6.7 1.9 7.5 5.5',
  info: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 11v5.5M12 7.8v.1',
  activity: 'M3.5 12h3.5l2.5-6 5 12 2.5-6h3.5',
  transfers: 'M7 19V6M3.5 9.5L7 6l3.5 3.5M17 5v13M13.5 14.5L17 18l3.5-3.5',
  close: 'M6 6l12 12M18 6L6 18',
  external: 'M14 4.5h5.5V10M19.5 4.5L11 13M10 5.5H5.5v13h13V14',
  globe: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM3.5 12h17M12 3c2.4 2.5 3.5 5.5 3.5 9s-1.1 6.5-3.5 9c-2.4-2.5-3.5-5.5-3.5-9S9.6 5.5 12 3z',
  drive: 'M3.5 14.5l2.3-8.2A1.5 1.5 0 0 1 7.2 5.2h9.6a1.5 1.5 0 0 1 1.4 1.1l2.3 8.2M3.5 14.5v3.5a1.5 1.5 0 0 0 1.5 1.5h14a1.5 1.5 0 0 0 1.5-1.5v-3.5zM16.5 17h.1',
  trash: 'M4.5 7h15M9.5 7V4.5h5V7M6.5 7l1 13h9l1-13',
  copy: 'M8.5 8.5h11v11h-11zM15.5 8.5V4.5h-11v11h4',
  chevronRight: 'M9.5 5.5L16 12l-6.5 6.5',
  chevronLeft: 'M14.5 5.5L8 12l6.5 6.5',
  clock: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 7v5l3.5 2',
  shield: 'M12 3.5l7 2.5v6c0 4.2-3 7.4-7 8.5-4-1.1-7-4.3-7-8.5V6z',
  hold: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM9 9l6 6M15 9l-6 6',
  skip: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM8 12.2l2.8 2.8L16 9.5',
  send: 'M4 12l16-7.5-6 15.5-2.6-6.4zM11.4 13.6L20 4.5',
  lifebuoy: 'M12 21a9 9 0 1 0 0-18 9 9 0 0 0 0 18zM12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7zM5.6 5.6l3.9 3.9M14.5 14.5l3.9 3.9M18.4 5.6l-3.9 3.9M9.5 14.5l-3.9 3.9',
  sparkle: 'M12 3.5l1.9 5.6 5.6 1.9-5.6 1.9L12 18.5l-1.9-5.6L4.5 11l5.6-1.9z',
  key: 'M14.5 13.5a4.5 4.5 0 1 0-4-4.5l-6 6v3h3v-2h2v-2h2z',
  plug: 'M9 3.5V8M15 3.5V8M6.5 8h11v3a5.5 5.5 0 0 1-11 0zM12 16.5v4',
} as const

export type IconName = keyof typeof paths

export function Icon({
  name,
  size = 18,
  label,
  ...rest
}: { name: IconName; size?: number; label?: string } & Omit<SVGProps<SVGSVGElement>, 'name'>) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.6}
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden={label ? undefined : true}
      role={label ? 'img' : undefined}
      aria-label={label}
      focusable="false"
      {...rest}
    >
      <path d={paths[name]} fill={name === 'play' ? 'currentColor' : 'none'} />
    </svg>
  )
}
