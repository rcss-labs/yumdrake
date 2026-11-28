import type { ReactNode } from 'react'
import { cx } from './classnames'
import './Icon.css'

export type IconName =
  | 'overview'
  | 'jobs'
  | 'runs'
  | 'approvals'
  | 'queue'
  | 'versions'
  | 'inventory'
  | 'cli'
  | 'admin'
  | 'search'
  | 'folder'

const drawings: Record<IconName, ReactNode> = {
  overview: (
    <>
      <rect x="3" y="3" width="7" height="9" rx="1" />
      <rect x="14" y="3" width="7" height="5" rx="1" />
      <rect x="14" y="12" width="7" height="9" rx="1" />
      <rect x="3" y="16" width="7" height="5" rx="1" />
    </>
  ),
  jobs: <path d="M8 6h13M8 12h13M8 18h13M3 6h.01M3 12h.01M3 18h.01" />,
  runs: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M12 7v5l3 2" />
    </>
  ),
  approvals: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="m9 12 2 2 4-4" />
    </>
  ),
  queue: <path d="M4 6h16M4 12h10M4 18h6" />,
  versions: (
    <>
      <rect x="3" y="3" width="18" height="18" rx="2" />
      <path d="M3 9h18M3 15h18M9 3v18" />
    </>
  ),
  inventory: (
    <>
      <rect x="2" y="3" width="20" height="7" rx="2" />
      <rect x="2" y="14" width="20" height="7" rx="2" />
      <path d="M6 6.5h.01M6 17.5h.01" />
    </>
  ),
  cli: <path d="m4 17 6-6-6-6M12 19h8" />,
  admin: <path d="M12 3l7 3v5c0 4.5-3 8-7 10-4-2-7-5.5-7-10V6l7-3z" />,
  search: (
    <>
      <circle cx="11" cy="11" r="7" />
      <path d="m20 20-3.5-3.5" />
    </>
  ),
  folder: <path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />,
}

export interface IconProps {
  name: IconName
  className?: string
}

export function Icon({ name, className }: IconProps) {
  return (
    <svg className={cx('ui-icon', className)} aria-hidden="true" focusable="false" viewBox="0 0 24 24">
      {drawings[name]}
    </svg>
  )
}
