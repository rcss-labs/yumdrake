import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, renderHook, waitFor } from '@testing-library/react'
import type { ReactNode } from 'react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ApiLogLine, LogPage, RunStateEvent } from '../../api/types'
import { runIds } from '../../test/fixtures'
import { delayNext, eventSources, failNext, mockState, requests } from '../../test/server'
import { REOPEN_DELAY_MS, useRunLive, WATCHDOG_MS } from './useRunLive'

function wrapper({ children }: { children: ReactNode }) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>
}

const line = (legId: number, seq: number, text = `line ${legId}/${seq}`, at = '2026-11-27T11:55:00Z'): ApiLogLine => ({
  leg_id: legId,
  seq,
  at,
  level: 'info',
  text,
})

const page = (lines: ApiLogLine[], next = `c${lines.length}`): LogPage => ({ lines, next, gaps: [] })

/** The state event the server would send for a run: the changing part of the detail, no steps. */
function stateEventOf(runId: number, changes: Partial<RunStateEvent> = {}): RunStateEvent {
  const run = mockState().runs.find((candidate) => candidate.id === runId)
  if (!run) throw new Error('no such run')
  return {
    id: run.id,
    state: run.state,
    wait_reason: run.wait_reason,
    end_reason: run.end_reason,
    state_changed_at: run.state_changed_at,
    started_at: run.started_at,
    finished_at: run.finished_at,
    cancel_requested: run.cancel_requested,
    legs: run.legs,
    locks: run.locks,
    summary: run.summary,
    ...changes,
  }
}

const logRequests = (runId: number) => requests.filter((request) => request.path === `/api/v1/runs/${runId}/log`)

/** Renders the hook for an active run and waits until the page opened its stream. */
async function watchActiveRun(runId: number, logs = true) {
  const view = renderHook(() => useRunLive(runId, { logs }), { wrapper })
  await waitFor(() => expect(eventSources.last()).toBeDefined())
  return view
}

describe('useRunLive on a finished run', () => {
  it('reads every page of 1000 lines in order, then reports finished, without opening a stream', async () => {
    const lines = Array.from({ length: 2500 }, (_, index) => line(0, index + 1, `run line ${index + 1}`))
    mockState().logs[runIds.succeeded] = lines
    const { result } = renderHook(() => useRunLive(runIds.succeeded, { logs: true }), { wrapper })

    await waitFor(() => expect(result.current.connection).toBe('finished'))
    expect(result.current.run?.id).toBe(runIds.succeeded)
    // Go sends null for a run without locks; the client hands the screens [].
    expect(result.current.run?.locks).toEqual([])
    expect(result.current.lines).toHaveLength(2500)
    expect(result.current.lines.map((l) => l.seq)).toEqual(lines.map((l) => l.seq))
    expect(logRequests(runIds.succeeded).map((request) => request.search)).toEqual([
      '?limit=1000',
      '?limit=1000&after=c1000',
      '?limit=1000&after=c2000',
    ])
    expect(eventSources.all).toHaveLength(0)
  })

  it('maps the API line to camelCase with the run id, and keeps leg 0 as the run-level lines', async () => {
    mockState().logs[runIds.succeeded] = [line(0, 1, 'created'), line(10901, 1, 'started')]
    const { result } = renderHook(() => useRunLive(runIds.succeeded, { logs: true }), { wrapper })
    await waitFor(() => expect(result.current.connection).toBe('finished'))
    expect(result.current.lines[0]).toEqual({
      runId: runIds.succeeded,
      legId: 0,
      seq: 1,
      at: '2026-11-27T11:55:00Z',
      level: 'info',
      text: 'created',
    })
    expect(result.current.lines[1]?.legId).toBe(10901)
  })

  it('makes no log request and reports no-access when logs are not wanted', async () => {
    const { result } = renderHook(() => useRunLive(runIds.succeeded, { logs: false }), { wrapper })
    await waitFor(() => expect(result.current.run).toBeDefined())
    expect(result.current.connection).toBe('no-access')
    expect(logRequests(runIds.succeeded)).toHaveLength(0)
  })

  it('sets the error, and opens no stream, when the run itself is refused', async () => {
    failNext('GET', `/api/v1/runs/${runIds.running}`, 403, 'forbidden')
    const { result } = renderHook(() => useRunLive(runIds.running, { logs: true }), { wrapper })
    await waitFor(() => expect(result.current.error?.status).toBe(403))
    expect(result.current.run).toBeUndefined()
    expect(eventSources.all).toHaveLength(0)
  })

  it('sets the error for a run that does not exist', async () => {
    const { result } = renderHook(() => useRunLive(9999, { logs: true }), { wrapper })
    await waitFor(() => expect(result.current.error?.status).toBe(404))
  })

  it('keeps the run when a log page fails, and retry reads the whole log again and clears the error', async () => {
    mockState().logs[runIds.succeeded] = [line(0, 1, 'created'), line(0, 2, 'done')]
    failNext('GET', `/api/v1/runs/${runIds.succeeded}/log`, 500, 'internal', 'log store down')
    const { result } = renderHook(() => useRunLive(runIds.succeeded, { logs: true }), { wrapper })
    await waitFor(() => expect(result.current.error?.message).toBe('log store down'))
    expect(result.current.run?.id).toBe(runIds.succeeded)
    expect(result.current.lines).toHaveLength(0)
    act(() => result.current.retry())
    await waitFor(() => expect(result.current.lines).toHaveLength(2))
    expect(result.current.error).toBeUndefined()
    expect(result.current.run?.id).toBe(runIds.succeeded)
  })
})

describe('useRunLive on an active run', () => {
  it('loads the run, opens the stream, applies a state event (keeping the steps), then appends log events', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const source = eventSources.last()!
    expect(source.url).toBe(`/api/v1/runs/${runIds.running}/events`)
    expect(result.current.run?.steps.map((step) => step.name)).toEqual(['copy-config', 'deploy'])
    expect(logRequests(runIds.running)).toHaveLength(0)

    act(() => {
      source.open()
      source.emit('state', stateEventOf(runIds.running, { state: 'cancelling', wait_reason: 'Waiting for services', cancel_requested: true }))
    })
    expect(result.current.run?.state).toBe('cancelling')
    expect(result.current.run?.cancel_requested).toBe(true)
    // The event carries no steps, so the loaded ones stay.
    expect(result.current.run?.steps).toHaveLength(2)

    act(() => {
      source.emit('log', page([line(0, 1, 'first'), line(10401, 1, 'second', '2026-11-27T11:55:05Z')]), 'c2')
    })
    expect(result.current.lines.map((l) => l.text)).toEqual(['first', 'second'])
    expect(result.current.connection).toBe('live')
  })

  it('adds nothing when the same log batch arrives again', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const source = eventSources.last()!
    const batch = page([line(0, 1), line(10401, 1), line(10401, 2)])
    act(() => source.emit('log', batch, 'c3'))
    act(() => source.emit('log', batch, 'c3'))
    expect(result.current.lines).toHaveLength(3)
  })

  it('shows reconnecting after the connection drops, then live again, with no line repeated or lost', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const source = eventSources.last()!
    act(() => {
      source.open()
      source.emit('log', page([line(0, 1), line(10401, 1)]), 'c2')
    })
    expect(result.current.connection).toBe('live')

    act(() => source.drop())
    expect(result.current.connection).toBe('reconnecting')

    // The browser reconnects the same EventSource; the server resends from the cursor and may repeat a line.
    act(() => {
      source.open()
      source.emit('log', page([line(10401, 1), line(10401, 2)]), 'c3')
    })
    expect(result.current.connection).toBe('live')
    expect(result.current.lines.map((l) => `${l.legId}/${l.seq}`)).toEqual(['0/1', '10401/1', '10401/2'])
  })

  it('shows live again on the next event even without an open event', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const source = eventSources.last()!
    act(() => source.drop())
    expect(result.current.connection).toBe('reconnecting')
    act(() => source.emit('state', stateEventOf(runIds.running)))
    expect(result.current.connection).toBe('live')
  })

  it('opens a new stream from the last cursor when the browser gives up', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const first = eventSources.last()!
    act(() => first.emit('log', page([line(0, 1)]), 'c1'))
    act(() => first.fail())
    expect(result.current.connection).toBe('reconnecting')
    await waitFor(() => expect(eventSources.all).toHaveLength(2), { timeout: 5000 })
    expect(eventSources.last()!.url).toBe(`/api/v1/runs/${runIds.running}/events?after=c1`)
  })

  it('stops reopening and shows the error when the run is refused after the browser gave up', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const first = eventSources.last()!
    act(() => first.open())
    failNext('GET', `/api/v1/runs/${runIds.running}`, 403, 'forbidden')
    act(() => first.fail())
    await waitFor(() => expect(result.current.error?.status).toBe(403), { timeout: 5000 })
    expect(eventSources.all).toHaveLength(1)
  })

  it('keeps retrying through a 503 from a proxy and goes live again', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const first = eventSources.last()!
    act(() => first.open())
    failNext('GET', `/api/v1/runs/${runIds.running}`, 503, 'unavailable')
    act(() => first.fail())
    await waitFor(() => expect(eventSources.all).toHaveLength(2), { timeout: 10_000 })
    expect(result.current.error).toBeUndefined()
    act(() => eventSources.last()!.open())
    expect(result.current.connection).toBe('live')
  }, 15_000)

  it('keeps the stream and the run when refreshing the detail fails, and refreshes on the next change', async () => {
    const { result } = await watchActiveRun(runIds.waitingApproval)
    const source = eventSources.last()!
    const stored = mockState().runs.find((candidate) => candidate.id === runIds.waitingApproval)!
    stored.approval = { ...stored.approval!, decision: 'approved' }
    failNext('GET', `/api/v1/runs/${runIds.waitingApproval}`, 503, 'unavailable')
    act(() => source.emit('state', stateEventOf(runIds.waitingApproval, { state: 'queued' })))
    await waitFor(() => expect(requests.filter((r) => r.path === `/api/v1/runs/${runIds.waitingApproval}` && r.status === 503)).toHaveLength(1))
    expect(result.current.error).toBeUndefined()
    expect(result.current.connection).toBe('live')
    expect(result.current.run?.state).toBe('queued')
    act(() => source.emit('state', stateEventOf(runIds.waitingApproval, { state: 'queued', wait_reason: 'next' })))
    await waitFor(() => expect(result.current.run?.approval?.decision).toBe('approved'))
  })

  it('drops a cursor the server refused at once and reads the log again before reopening', async () => {
    mockState().logs[runIds.running] = [line(0, 1, 'held'), line(10401, 1, 'also held')]
    const { result } = await watchActiveRun(runIds.running)
    const first = eventSources.last()!
    act(() => first.emit('log', page([line(0, 1, 'held')]), 'c1'))
    const reopened = await (async () => {
      // A new stream that is refused before it says anything (a 422 for a bad cursor) closes at once.
      act(() => first.fail())
      await waitFor(() => expect(eventSources.all).toHaveLength(2), { timeout: 5000 })
      return eventSources.last()!
    })()
    expect(reopened.url).toBe(`/api/v1/runs/${runIds.running}/events?after=c1`)
    act(() => reopened.fail())
    await waitFor(() => expect(eventSources.all).toHaveLength(3), { timeout: 8000 })
    expect(eventSources.last()!.url).toBe(`/api/v1/runs/${runIds.running}/events`)
    expect(logRequests(runIds.running).length).toBeGreaterThan(0)
    expect(result.current.lines.map((l) => l.text)).toEqual(['held', 'also held'])
  }, 15_000)

  it('refreshes the approval when the state event says another person decided', async () => {
    const { result } = await watchActiveRun(runIds.waitingApproval)
    expect(result.current.run?.approval?.decision).toBe('pending')
    const stored = mockState().runs.find((candidate) => candidate.id === runIds.waitingApproval)!
    stored.state = 'queued'
    stored.approval = { ...stored.approval!, decision: 'approved', decided_by_name: 'Gal Grantee', decided_at: '2026-11-27T12:00:00Z' }
    act(() => eventSources.last()!.emit('state', stateEventOf(runIds.waitingApproval)))
    await waitFor(() => expect(result.current.run?.approval?.decision).toBe('approved'))
    expect(result.current.run?.state).toBe('queued')
  })

  it('reload fetches the run again and replaces it, keeping the lines', async () => {
    const { result } = await watchActiveRun(runIds.running)
    act(() => eventSources.last()!.emit('log', page([line(0, 1, 'kept')]), 'c1'))
    mockState().runs.find((candidate) => candidate.id === runIds.running)!.wait_reason = 'Changed meanwhile'
    const before = requests.filter((request) => request.path === `/api/v1/runs/${runIds.running}`).length
    act(() => result.current.reload())
    await waitFor(() => expect(result.current.run?.wait_reason).toBe('Changed meanwhile'))
    expect(requests.filter((request) => request.path === `/api/v1/runs/${runIds.running}`).length).toBe(before + 1)
    expect(result.current.lines.map((l) => l.text)).toEqual(['kept'])
  })

  it('does not let a reload that was asked before a state event overwrite that event', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const stored = mockState().runs.find((candidate) => candidate.id === runIds.running)!
    // The reload is read from the server's old state and answers late.
    delayNext('GET', `/api/v1/runs/${runIds.running}`, 300)
    act(() => result.current.reload())
    await waitFor(() => expect(requests.filter((request) => request.path === `/api/v1/runs/${runIds.running}`)).toHaveLength(2))
    act(() => eventSources.last()!.emit('state', stateEventOf(runIds.running, { wait_reason: 'Newer than the reload' })))
    expect(result.current.run?.wait_reason).toBe('Newer than the reload')
    await waitFor(() => expect(requests.filter((request) => request.path === `/api/v1/runs/${runIds.running}` && request.status === 200)).toHaveLength(2))
    // The old answer has arrived (the stored run still says the old reason); the event's state stays.
    expect(stored.wait_reason).not.toBe('Newer than the reload')
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 20))
    })
    expect(result.current.run?.wait_reason).toBe('Newer than the reload')
  })

  describe('the silence watchdog', () => {
    // Only the timers: the mock server and Testing Library's polling keep using real ones.
    const advance = (ms: number) => act(async () => { await vi.advanceTimersByTimeAsync(ms) })
    // Testing Library's waitFor waits on a timer that is faked here, so these tests wait with vitest's own.
    const watchWithFakeTimers = async () => {
      renderHook(() => useRunLive(runIds.running, { logs: true }), { wrapper })
      await vi.waitFor(() => expect(eventSources.last()).toBeDefined())
    }
    const runFetches = () => requests.filter((request) => request.path === `/api/v1/runs/${runIds.running}`).length

    afterEach(() => {
      vi.useRealTimers()
    })

    it('reconnects when the stream is silent for 45 seconds', async () => {
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      await watchWithFakeTimers()
      const first = eventSources.last()!
      act(() => first.open())
      const fetchesBefore = runFetches()
      await advance(WATCHDOG_MS - 1000)
      expect(first.closed).toBe(false)
      await advance(1000)
      expect(first.closed).toBe(true)
      // The watchdog itself reads the run (reload) before it schedules the reopen.
      await vi.waitFor(() => expect(runFetches()).toBe(fetchesBefore + 1))
      // The existing reconnect path: a pause, then the run is read again and a new stream opens.
      await advance(REOPEN_DELAY_MS)
      await vi.waitFor(() => expect(eventSources.all).toHaveLength(2))
      expect(runFetches()).toBeGreaterThan(fetchesBefore)
    })

    it('a ping keeps the stream alive', async () => {
      vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] })
      await watchWithFakeTimers()
      const stream = eventSources.last()!
      act(() => stream.open())
      for (let beat = 0; beat < 4; beat += 1) {
        await advance(WATCHDOG_MS - 5000)
        act(() => stream.emit('ping', {}))
      }
      expect(stream.closed).toBe(false)
      expect(eventSources.all).toHaveLength(1)
    })
  })

  it('closes the stream and reports finished on the end event', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const source = eventSources.last()!
    // Go sends null for the locks of a run that holds none.
    const ended = stateEventOf(runIds.running, { state: 'succeeded', finished_at: '2026-11-27T12:00:00Z', locks: null as unknown as string[] })
    act(() => source.emit('end', ended))
    expect(source.closed).toBe(true)
    expect(result.current.connection).toBe('finished')
    expect(result.current.run?.state).toBe('succeeded')
    expect(result.current.run?.locks).toEqual([])
  })

  it('closes the stream when the page goes away', async () => {
    const { unmount } = await watchActiveRun(runIds.running)
    const source = eventSources.last()!
    unmount()
    expect(source.closed).toBe(true)
  })

  it('opens the stream but ignores log events and makes no log request when logs are not wanted', async () => {
    const { result } = await watchActiveRun(runIds.running, false)
    const source = eventSources.last()!
    act(() => {
      source.emit('state', stateEventOf(runIds.running))
      source.emit('log', page([line(0, 1)]), 'c1')
    })
    expect(result.current.lines).toHaveLength(0)
    expect(result.current.connection).toBe('no-access')
    expect(logRequests(runIds.running)).toHaveLength(0)
  })

  it('keeps the newest 20,000 lines and says it dropped the rest', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const source = eventSources.last()!
    const total = 20_500
    const lines = Array.from({ length: total }, (_, index) => line(10401, index + 1, `n${index + 1}`, new Date(Date.parse('2026-11-27T10:00:00Z') + index * 1000).toISOString()))
    act(() => {
      for (let from = 0; from < total; from += 500) {
        source.emit('log', page(lines.slice(from, from + 500)), `c${from + 500}`)
      }
    })
    expect(result.current.truncated).toBe(true)
    expect(result.current.lines).toHaveLength(20_000)
    expect(result.current.lines[0]?.text).toBe('n501')
    expect(result.current.lines.at(-1)?.text).toBe(`n${total}`)
  })

  it('orders lines by time, then leg, then seq', async () => {
    const { result } = await watchActiveRun(runIds.running)
    const source = eventSources.last()!
    act(() =>
      source.emit(
        'log',
        page([
          line(10402, 1, 'b', '2026-11-27T11:55:02Z'),
          line(10401, 2, 'a2', '2026-11-27T11:55:01Z'),
          line(10401, 1, 'a1', '2026-11-27T11:55:01Z'),
          line(10402, 2, 'c', '2026-11-27T11:55:03Z'),
        ]),
        'c4',
      ),
    )
    expect(result.current.lines.map((l) => l.text)).toEqual(['a1', 'a2', 'b', 'c'])
  })

  it('collects the gaps the log reports', async () => {
    mockState().logGaps[runIds.succeeded] = [{ leg_id: 10901, from_seq: 4, to_seq: 9 }]
    const { result } = renderHook(() => useRunLive(runIds.succeeded, { logs: true }), { wrapper })
    await waitFor(() => expect(result.current.connection).toBe('finished'))
    expect(result.current.gaps).toEqual([{ legId: 10901, fromSeq: 4, toSeq: 9 }])
  })
})
