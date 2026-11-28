import { Note, relativeTime } from '../../ui'

interface RefreshNoteProps {
  /** True when the latest refresh failed although there is older data to show. */
  failed: boolean
  /** When the data on screen was fetched, in milliseconds. */
  fetchedAt: number
}

/** A small muted note for a polling screen whose refresh failed; the screen keeps showing the last data. */
export function RefreshNote({ failed, fetchedAt }: RefreshNoteProps) {
  if (!failed) return null
  return <Note className="queue-stale">Could not refresh. Showing data from {relativeTime(new Date(fetchedAt).toISOString())}.</Note>
}
