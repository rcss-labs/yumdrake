import type { ReactNode } from 'react'
import './StatTile.css'

export interface StatTileProps {
  label: string
  value?: ReactNode
  /** Shown in place of the value when there is none, for example "No runs in this period". */
  empty?: string
  /** What the number is compared with, in words. */
  comparison?: ReactNode
}

export function StatTile({ label, value, empty, comparison }: StatTileProps) {
  const hasValue = value !== undefined && value !== null
  return (
    <div className="ui-tile">
      <div className="ui-tile-label">{label}</div>
      {hasValue ? <div className="ui-tile-value num">{value}</div> : <div className="ui-tile-empty">{empty}</div>}
      {comparison && <div className="ui-tile-comparison">{comparison}</div>}
    </div>
  )
}
