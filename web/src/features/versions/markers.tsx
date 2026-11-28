// The three things worth noticing about a running version. Each is a shape plus words, never colour alone.
import type { RunningCell } from '../../api/types'
import './versions.css'

export type MarkerKey = 'behind' | 'failed' | 'outside'

/** What each marker is called in the legend and in a row's detail, and what a cell says in short. */
export const markerText: Record<MarkerKey, { legend: string; short: string }> = {
  behind: { legend: 'Behind another environment with the same label', short: 'behind' },
  failed: { legend: 'Last deploy failed', short: 'last deploy failed' },
  outside: { legend: 'Not deployed by Yumdrake', short: 'not deployed by Yumdrake' },
}

/** The markers of a cell, in a fixed order. Only a cell that was read has any. */
export function markersOf(cell: RunningCell): MarkerKey[] {
  if (cell.state !== 'ok') return []
  const markers: MarkerKey[] = []
  if (cell.behind) markers.push('behind')
  if (cell.last_deploy_failed) markers.push('failed')
  if (!cell.by_yumdrake) markers.push('outside')
  return markers
}

function Shape({ marker }: { marker: MarkerKey }) {
  return (
    <svg className="versions-shape" data-marker={marker} viewBox="0 0 12 12" aria-hidden="true" focusable="false">
      {marker === 'behind' && <path d="M6 2v8M2.5 6.5 6 10l3.5-3.5" />}
      {marker === 'failed' && <path d="M2.5 2.5l7 7M9.5 2.5l-7 7" />}
      {marker === 'outside' && <circle cx="6" cy="6" r="3.5" />}
    </svg>
  )
}

/** A marker inside a cell: the shape, with its short words for assistive technology. */
export function Marker({ marker }: { marker: MarkerKey }) {
  return (
    <span className="versions-mark" title={markerText[marker].legend}>
      <Shape marker={marker} />
      <span className="versions-sr">{markerText[marker].short}</span>
    </span>
  )
}

/** A marker with its words on screen, for the legend and a row's detail. */
export function MarkerWithWords({ marker }: { marker: MarkerKey }) {
  return (
    <span className="versions-mark-words">
      <Shape marker={marker} />
      {markerText[marker].legend}
    </span>
  )
}
