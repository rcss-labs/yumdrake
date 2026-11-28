// Everything the run page knows about one run: the run itself, its log lines and how the live connection is.
//
// A finished run is read once: GET the run, then the whole log in pages of 1000. An active run is watched:
// GET the run, then an EventSource on its stream. `state` events change the run (they carry no steps, so they
// are merged into what was loaded), `log` events append lines, `end` closes the stream. If the connection
// breaks the browser reconnects by itself with Last-Event-ID; if it gives up (readyState CLOSED) this hook
// opens a new stream after a pause, from the last cursor it saw. A line already held (same leg and seq) is
// never added twice, so a resent batch changes nothing.
import { useCallback, useEffect, useReducer, useRef, useState } from 'react'
import { api, nullListsToEmpty } from '../../api/client'
import { ApiError } from '../../api/error'
import type {
  ApiLogGap,
  ApiLogLine,
  LogGap,
  LogLine,
  LogPage,
  RunConnection,
  RunDetail,
  RunLive,
  RunStateEvent,
} from '../../api/types'
import { isFinalRunState } from '../../api/types'

export type { RunLive }

export const MAX_LINES = 20_000
export const LOG_PAGE_SIZE = 1000
/** How long to wait before opening a new stream after the browser gave up on one. */
export const REOPEN_DELAY_MS = 2000
/** The pause before reopening doubles on every failure up to this. */
export const REOPEN_MAX_DELAY_MS = 30_000
/** The server pings every 15 s; no event of any kind for three pings means the stream is dead. */
export const WATCHDOG_MS = 45_000

interface State {
  run: RunDetail | undefined
  lines: LogLine[]
  gaps: LogGap[]
  connection: RunConnection
  truncated: boolean
  error: ApiError | undefined
}

type Action =
  // `keepRun`: a retry starts over without blanking the page.
  | { type: 'reset'; keepRun: boolean }
  | { type: 'loaded'; run: RunDetail }
  // A refetched run: `detailOnly` keeps the streamed fields and takes the rest (approval, selection, ...).
  | { type: 'detail'; run: RunDetail; detailOnly: boolean }
  | { type: 'state'; event: RunStateEvent }
  | { type: 'lines'; lines: LogLine[]; gaps: LogGap[] }
  | { type: 'connection'; connection: RunConnection }
  | { type: 'error'; error: ApiError }

const initial: State = { run: undefined, lines: [], gaps: [], connection: 'live', truncated: false, error: undefined }

const timeOf = (line: LogLine) => Date.parse(line.at)

/** Time, then leg, then seq. */
function compareLines(a: LogLine, b: LogLine): number {
  return timeOf(a) - timeOf(b) || a.legId - b.legId || a.seq - b.seq
}

function mergeLines(existing: LogLine[], added: LogLine[]): { lines: LogLine[]; truncated: boolean } {
  const last = existing[existing.length - 1]
  const sortedAdded = [...added].sort(compareLines)
  const first = sortedAdded[0]
  const inOrder = last === undefined || first === undefined || compareLines(last, first) <= 0
  let merged = inOrder ? existing.concat(sortedAdded) : existing.concat(sortedAdded).sort(compareLines)
  let truncated = false
  if (merged.length > MAX_LINES) {
    merged = merged.slice(merged.length - MAX_LINES)
    truncated = true
  }
  return { lines: merged, truncated }
}

function reducer(state: State, action: Action): State {
  switch (action.type) {
    case 'reset':
      return action.keepRun ? { ...initial, run: state.run } : initial
    case 'loaded':
      return { ...state, run: action.run }
    case 'detail': {
      if (!action.detailOnly || !state.run) return { ...state, run: action.run }
      const { id, state: runState, wait_reason, end_reason, state_changed_at, started_at, finished_at, cancel_requested, legs, locks, summary } = state.run
      return {
        ...state,
        run: { ...action.run, id, state: runState, wait_reason, end_reason, state_changed_at, started_at, finished_at, cancel_requested, legs, locks, summary },
      }
    }
    case 'state':
      // The event has no steps, selection or approval: keep what was loaded.
      return state.run ? { ...state, run: { ...state.run, ...action.event }, connection: state.connection === 'finished' ? 'finished' : 'live' } : state
    case 'lines': {
      const merged = action.lines.length > 0 ? mergeLines(state.lines, action.lines) : { lines: state.lines, truncated: false }
      const known = new Set(state.gaps.map((gap) => `${gap.legId}:${gap.fromSeq}:${gap.toSeq}`))
      const gaps = state.gaps.concat(action.gaps.filter((gap) => !known.has(`${gap.legId}:${gap.fromSeq}:${gap.toSeq}`)))
      return {
        ...state,
        lines: merged.lines,
        gaps,
        truncated: state.truncated || merged.truncated,
        connection: state.connection === 'finished' ? 'finished' : 'live',
      }
    }
    case 'connection':
      return { ...state, connection: action.connection }
    case 'error':
      return { ...state, error: action.error, connection: 'finished' }
  }
}

const toLine = (runId: number) => (line: ApiLogLine): LogLine => ({
  runId,
  legId: line.leg_id,
  seq: line.seq,
  at: line.at,
  level: line.level,
  text: line.text,
})

const toGap = (gap: ApiLogGap): LogGap => ({ legId: gap.leg_id, fromSeq: gap.from_seq, toSeq: gap.to_seq })

function asApiError(caught: unknown): ApiError {
  return caught instanceof ApiError ? caught : new ApiError(0, 'network', 'Something went wrong. Try again.')
}

/** The server said no (4xx): retrying will not help. No answer, a timeout or a 5xx (a proxy during a restart) will pass. */
const isRefusal = (error: ApiError) => error.status >= 400 && error.status < 500

/** The body of a stream event, with the nulls Go sends for empty lists turned into []. */
function parse<T>(event: Event): T | undefined {
  try {
    return nullListsToEmpty(JSON.parse((event as MessageEvent<string>).data)) as T
  } catch {
    return undefined
  }
}

export function useRunLive(runId: number, opts: { logs: boolean }): RunLive {
  const [state, dispatch] = useReducer(reducer, initial)
  const wantLogs = opts.logs
  const reloadRef = useRef<(() => void) | undefined>(undefined)
  // Bumped by retry(): the effect below runs again and starts over.
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    let cancelled = false
    let source: EventSource | undefined
    let reopenTimer: ReturnType<typeof setTimeout> | undefined
    let watchdog: ReturnType<typeof setTimeout> | undefined
    // (legId, seq) of every line taken so far, also of lines the 20,000 cap dropped.
    const seen = new Set<string>()
    let cursor = ''
    dispatch({ type: 'reset', keepRun: attempt > 0 })

    const takeLines = (page: LogPage) => {
      const fresh: LogLine[] = []
      for (const line of page.lines) {
        const key = `${line.leg_id}:${line.seq}`
        if (!seen.has(key)) {
          seen.add(key)
          fresh.push(toLine(runId)(line))
        }
      }
      dispatch({ type: 'lines', lines: fresh, gaps: page.gaps.map(toGap) })
    }

    const readWholeLog = async () => {
      let after = ''
      for (;;) {
        const query = `limit=${LOG_PAGE_SIZE}${after === '' ? '' : `&after=${encodeURIComponent(after)}`}`
        const page = await api<LogPage>('GET', `/api/v1/runs/${runId}/log?${query}`)
        if (cancelled) return
        takeLines(page)
        if (page.lines.length < LOG_PAGE_SIZE || page.next === '' || page.next === after) return
        after = page.next
      }
    }

    // What the last state the page knows said, to notice a change that the stream's state event cannot describe fully.
    let knownState: RunDetail['state'] | undefined
    let knownCancel = false
    // Reopening: failures so far (for the backoff), and whether the log must be read again to fill what a dropped cursor lost.
    let failures = 0
    let refillLog = false

    const fetchRun = () => api<RunDetail>('GET', `/api/v1/runs/${runId}`)

    // Gets the detail-only fields (approval, selection, ...) that a state event does not carry.
    // Bumped by every state event applied. An answer that was asked before a newer event must not replace what that
    // event said, so it is then taken as detail only (a counter, because event times can be equal).
    let eventsApplied = 0
    const refreshDetail = async (detailOnly: boolean) => {
      const askedAt = eventsApplied
      try {
        const run = await fetchRun()
        if (!cancelled) dispatch({ type: 'detail', run, detailOnly: detailOnly || eventsApplied !== askedAt })
      } catch (caught) {
        const error = asApiError(caught)
        if (cancelled) return
        // A refusal ends the page's view of the run; anything else (network, 5xx) keeps the stream and the run
        // as they are, and the next state change tries the refresh again.
        if (isRefusal(error)) dispatch({ type: 'error', error })
        else knownState = undefined
      }
    }
    reloadRef.current = () => void refreshDetail(false)

    const applyState = (body: RunStateEvent) => {
      eventsApplied += 1
      dispatch({ type: 'state', event: body })
      if (body.state !== knownState || body.cancel_requested !== knownCancel) {
        knownState = body.state
        knownCancel = body.cancel_requested
        void refreshDetail(true)
      }
    }

    const openStream = () => {
      const suffix = cursor === '' ? '' : `?after=${encodeURIComponent(cursor)}`
      const stream = new EventSource(`/api/v1/runs/${runId}/events${suffix}`)
      source = stream
      let heardFromServer = false
      // No event of any kind (a ping counts) for WATCHDOG_MS: the stream is dead though the browser has not said so.
      // Drop it, read the run again, and reopen through the same path as a stream the browser gave up on.
      const armWatchdog = () => {
        clearTimeout(watchdog)
        watchdog = setTimeout(() => {
          if (cancelled || source !== stream) return
          stream.close()
          dispatch({ type: 'connection', connection: 'reconnecting' })
          reloadRef.current?.()
          scheduleReopen()
        }, WATCHDOG_MS)
      }
      const heard = () => {
        heardFromServer = true
        failures = 0
        armWatchdog()
      }
      armWatchdog()
      stream.addEventListener('ping', heard)
      stream.addEventListener('open', () => {
        heard()
        dispatch({ type: 'connection', connection: 'live' })
      })
      stream.addEventListener('state', (event) => {
        heard()
        const body = parse<RunStateEvent>(event)
        if (body) applyState(body)
      })
      stream.addEventListener('log', (event) => {
        heard()
        const body = parse<LogPage>(event)
        if (!body) return
        const id = (event as MessageEvent).lastEventId
        if (id) cursor = id
        if (wantLogs) takeLines(body)
        else dispatch({ type: 'connection', connection: 'live' })
      })
      stream.addEventListener('end', (event) => {
        heard()
        const body = parse<RunStateEvent>(event)
        if (body) applyState(body)
        stream.close()
        clearTimeout(watchdog)
        dispatch({ type: 'connection', connection: 'finished' })
      })
      stream.addEventListener('error', () => {
        dispatch({ type: 'connection', connection: 'reconnecting' })
        // CONNECTING: the browser is retrying by itself. CLOSED: it gave up, so check the run and ask again later.
        if (stream.readyState === EventSource.CLOSED && !cancelled) {
          stream.close()
          clearTimeout(watchdog)
          // A stream that was refused at once with a cursor may have been given a cursor the server does not accept
          // (422): forget it and read the log again; lines already held are not added twice.
          if (!heardFromServer && cursor !== '') {
            cursor = ''
            refillLog = true
          }
          scheduleReopen()
        }
      })
    }

    // Finishes a run that is final: its log, then `finished`.
    const settleFinal = async () => {
      try {
        if (wantLogs) await readWholeLog()
      } catch (caught) {
        if (!cancelled) dispatch({ type: 'error', error: asApiError(caught) })
        return
      }
      if (!cancelled) dispatch({ type: 'connection', connection: 'finished' })
    }

    // Before a new stream, ask for the run through api(): a 401 reaches the sign-in handler, a 403 or 404 is shown
    // and stops the retrying, and only a network failure keeps trying (with a doubling pause).
    const scheduleReopen = () => {
      const delay = Math.min(REOPEN_DELAY_MS * 2 ** failures, REOPEN_MAX_DELAY_MS)
      failures += 1
      reopenTimer = setTimeout(async () => {
        let run: RunDetail
        try {
          run = await fetchRun()
        } catch (caught) {
          const error = asApiError(caught)
          if (cancelled) return
          if (isRefusal(error)) dispatch({ type: 'error', error })
          else scheduleReopen()
          return
        }
        if (cancelled) return
        dispatch({ type: 'detail', run, detailOnly: false })
        if (isFinalRunState(run.state)) {
          await settleFinal()
          return
        }
        if (refillLog && wantLogs) {
          try {
            await readWholeLog()
            refillLog = false
          } catch (caught) {
            const error = asApiError(caught)
            if (cancelled) return
            if (isRefusal(error)) dispatch({ type: 'error', error })
            else scheduleReopen()
            return
          }
        }
        if (!cancelled) openStream()
      }, delay)
    }

    const start = async () => {
      let run: RunDetail
      try {
        run = await fetchRun()
      } catch (caught) {
        if (!cancelled) dispatch({ type: 'error', error: asApiError(caught) })
        return
      }
      if (cancelled) return
      knownState = run.state
      knownCancel = run.cancel_requested
      dispatch({ type: 'loaded', run })
      if (isFinalRunState(run.state)) {
        await settleFinal()
        return
      }
      openStream()
    }

    void start()
    return () => {
      cancelled = true
      reloadRef.current = undefined
      if (reopenTimer) clearTimeout(reopenTimer)
      clearTimeout(watchdog)
      source?.close()
    }
  }, [runId, wantLogs, attempt])

  const reload = useCallback(() => reloadRef.current?.(), [])
  const retry = useCallback(() => setAttempt((count) => count + 1), [])

  return {
    run: state.run,
    lines: state.lines,
    gaps: state.gaps,
    connection: wantLogs ? state.connection : 'no-access',
    truncated: state.truncated,
    error: state.error,
    reload,
    retry,
  }
}
