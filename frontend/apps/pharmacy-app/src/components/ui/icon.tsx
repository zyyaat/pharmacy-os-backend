export type IconName =
  | 'dashboard'
  | 'package'
  | 'users'
  | 'calendar'
  | 'store'
  | 'chart'
  | 'settings'
  | 'bell'
  | 'search'
  | 'menu'
  | 'sun'
  | 'moon'
  | 'plus'
  | 'arrowUp'
  | 'arrowDown'
  | 'activity'
  | 'alert'
  | 'clock'
  | 'chevronLeft'
  | 'chevronRight'
  | 'logout'
  | 'x'
  | 'pill'

const paths: Record<IconName, string> = {
  dashboard: 'M3 3h7v7H3V3Zm11 0h7v7h-7V3ZM3 14h7v7H3v-7Zm11 0h7v7h-7v-7Z',
  package: 'm12 3 8 4.5v9L12 21l-8-4.5v-9L12 3Zm0 0v9m8-4.5-8 4.5m-8-4.5 8 4.5',
  users: 'M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2m6-10a4 4 0 1 0 0-8 4 4 0 0 0 0 8Zm10 10v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75',
  calendar: 'M4 5h16v15H4V5Zm4-2v4m8-4v4M4 9h16M8 13h.01M12 13h.01M16 13h.01M8 17h.01M12 17h.01',
  store: 'M3 10h18M5 10v10h14V10M3 10l2-6h14l2 6M8 20v-5h8v5',
  chart: 'M4 19V5m0 14h16M8 16v-4m4 4V8m4 8V5',
  settings: 'M12 15.5a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7Zm0-13v2m0 15v2M4.22 4.22l1.42 1.42m12.72 12.72 1.42 1.42M1 12h2m18 0h2M4.22 19.78l1.42-1.42M18.36 5.64l1.42-1.42',
  bell: 'M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9M10 21h4',
  search: 'm21 21-4.35-4.35m2.35-5.65a8 8 0 1 1-16 0 8 8 0 0 1 16 0Z',
  menu: 'M4 6h16M4 12h16M4 18h16',
  sun: 'M12 3v2m0 14v2M3 12h2m14 0h2M5.64 5.64l1.42 1.42m9.88 9.88 1.42 1.42M5.64 18.36l1.42-1.42m9.88-9.88 1.42-1.42M16 12a4 4 0 1 1-8 0 4 4 0 0 1 8 0Z',
  moon: 'M21 12.79A9 9 0 1 1 11.21 3 7 7 0 0 0 21 12.79Z',
  plus: 'M12 5v14M5 12h14',
  arrowUp: 'm5 12 7-7 7 7M12 19V5',
  arrowDown: 'm5 12 7 7 7-7M12 5v14',
  activity: 'M3 12h4l3-8 4 16 3-8h4',
  alert: 'M10.3 3.7 2.6 17a2 2 0 0 0 1.73 3h15.34A2 2 0 0 0 21.4 17L13.7 3.7a2 2 0 0 0-3.4 0ZM12 9v4m0 4h.01',
  clock: 'M12 7v5l3 2m6-2a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z',
  chevronLeft: 'm15 18-6-6 6-6',
  chevronRight: 'm9 18 6-6-6-6',
  logout: 'M10 17l5-5-5-5m5 5H3m12-7V4a2 2 0 0 0-2-2H5a2 2 0 0 0-2 2v2m0 12v2a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2v-3',
  x: 'M6 6l12 12M18 6 6 18',
  pill: 'm4.93 4.93 14.14 14.14m-2.12-12.02a4.5 4.5 0 1 1-6.36 6.36L5.64 17.36A4.5 4.5 0 0 1 12 11l4.95-4.95Z',
}

export function Icon({
  name,
  size = 20,
  className = '',
}: {
  name: IconName
  size?: number
  className?: string
}) {
  return (
    <svg
      aria-hidden="true"
      className={className}
      fill="none"
      height={size}
      viewBox="0 0 24 24"
      width={size}
      xmlns="http://www.w3.org/2000/svg"
    >
      <path
        d={paths[name]}
        stroke="currentColor"
        strokeLinecap="round"
        strokeLinejoin="round"
        strokeWidth="1.8"
      />
    </svg>
  )
}