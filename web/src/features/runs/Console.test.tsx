import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { MemoryRouter } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../../api/error'
import type { ApiLogLine, LogGap, LogLevel, LogLine, RunDetail, RunLive } from '../../api/types'
import { failNext, mockState, requests, runIds } from '../../test/server'
import { Console } from './Console'
import { StepsTree } from './StepsTree'
import { wholeRun, type ConsoleScope } from './scope'
import { useRunLive } from './useRunLive'

const runOf = (id: number): RunDetail => {
  const run = mockState().runs.find((candidate) => candidate.id === id)
  if (!run) throw new Error('no such run')
  return run
}

// Run 105 legs: 10501 resolve, 10502 revalidate, 10503/10504 copy-config backend-api/billing,
// 10505/10506 deploy, 10507/10508 verify. Leg 0 is a run-level line.
const line = (legId: number, seq: number, text: string, level: LogLevel = 'info', runId = 105): LogLine => ({
  runId,
  legId,
  seq,
  at: '2026-11-27T10:03:00Z',
  level,
  text,
})

const liveOf = (run: RunDetail, lines: LogLine[], extra: Partial<RunLive> = {}): RunLive => ({
  run,
  lines,
  gaps: [],
  connection: 'finished',
  truncated: false,
  error: undefined,
  reload: () => {},
  retry: () => {},
  ...extra,
})

const rowsOf = (container: HTMLElement) => Array.from(container.querySelectorAll('.con-ln')).map((row) => row.textContent ?? '')

const sample: LogLine[] = [
  line(0, 1, 'run started'),
  line(10503, 1, 'backend-api: copied'),
  line(10504, 1, 'billing: reading source'),
  line(10504, 2, 'billing: NoSuchKey', 'error'),
  line(10506, 1, 'billing: deploy skipped', 'warn'),
  line(10505, 1, 'backend-api: deployed', 'ok'),
]

describe('Console scope and counts', () => {
  const run = runOf(runIds.partiallySucceeded)

  it('shows every line in the whole run, with the counts in the header', () => {
    const { container } = render(<Console run={run} live={liveOf(run, sample)} scope={wholeRun} />)
    expect(rowsOf(container)).toHaveLength(6)
    expect(screen.getByText('Whole run')).toBeInTheDocument()
    expect(screen.getByText('6 of 6 lines, 1 error line')).toBeInTheDocument()
  })

  it('shows only the lines of a step', () => {
    const { container } = render(<Console run={run} live={liveOf(run, sample)} scope={{ kind: 'step', step: 'copy-config' }} />)
    expect(rowsOf(container).map((text) => text.replace(/^.*?(backend|billing)/, '$1'))).toEqual([
      'backend-api: copied',
      'billing: reading source',
      'billing: NoSuchKey',
    ])
    expect(screen.getByText('3 of 3 lines, 1 error line')).toBeInTheDocument()
  })

  it('shows a service with its lines from every step, including its config step', () => {
    const { container } = render(<Console run={run} live={liveOf(run, sample)} scope={{ kind: 'service', service: 'billing' }} />)
    const text = rowsOf(container).join('\n')
    expect(rowsOf(container)).toHaveLength(3)
    expect(text).toContain('billing: reading source')
    expect(text).toContain('billing: NoSuchKey')
    expect(text).toContain('billing: deploy skipped')
    expect(text).not.toContain('backend-api')
    expect(text).not.toContain('run started')
  })

  it('combines the text filter with errors only, and counts shown against all lines of the scope', async () => {
    const { container } = render(<Console run={run} live={liveOf(run, sample)} scope={{ kind: 'service', service: 'billing' }} />)
    await userEvent.type(screen.getByRole('searchbox', { name: 'Filter log lines' }), 'NOSUCH')
    await waitFor(() => expect(rowsOf(container)).toHaveLength(1))
    expect(screen.getByText('1 of 3 lines, 1 error line')).toBeInTheDocument()

    await userEvent.clear(screen.getByRole('searchbox', { name: 'Filter log lines' }))
    await userEvent.type(screen.getByRole('searchbox', { name: 'Filter log lines' }), 'billing')
    await userEvent.click(screen.getByRole('button', { name: 'Errors only' }))
    await waitFor(() => expect(rowsOf(container)).toHaveLength(1))
    expect(screen.getByRole('button', { name: 'Errors only' })).toHaveAttribute('aria-pressed', 'true')

    await userEvent.clear(screen.getByRole('searchbox', { name: 'Filter log lines' }))
    await userEvent.type(screen.getByRole('searchbox', { name: 'Filter log lines' }), 'deploy skipped')
    await waitFor(() => expect(screen.getByText('No lines match')).toBeInTheDocument())
  })

  it('numbers the lines of the scope, tints errors, and tells a step prefix in the whole run', () => {
    const { container } = render(<Console run={run} live={liveOf(run, sample)} scope={wholeRun} />)
    const rows = container.querySelectorAll('.con-ln')
    expect(rows[3]).toHaveClass('con-ln-error')
    expect(rows[4]).toHaveClass('con-ln-warn')
    expect(rows[5]).toHaveClass('con-ln-ok')
    expect(rows[3]?.querySelector('.con-no')).toHaveTextContent('4')
    expect(rows[3]?.textContent).toContain('[copy-config / billing]')
  })
})

describe('Console gaps', () => {
  it('renders a gap as its own row before the first line after it, and hides it while filtering', async () => {
    const run = runOf(runIds.partiallySucceeded)
    const gaps: LogGap[] = [{ legId: 10504, fromSeq: 2, toSeq: 5 }]
    const lines = [line(10504, 1, 'billing: first'), line(10504, 6, 'billing: after the gap')]
    const { container } = render(<Console run={run} live={liveOf(run, lines, { gaps })} scope={{ kind: 'step', step: 'copy-config' }} />)
    const rows = Array.from(container.querySelectorAll('.con-ln, .con-gap')).map((row) => row.textContent ?? '')
    expect(rows).toHaveLength(3)
    expect(rows[1]).toBe('Lines 2 to 5 of this step are missing')
    await userEvent.type(screen.getByRole('searchbox', { name: 'Filter log lines' }), 'first')
    await waitFor(() => expect(container.querySelector('.con-gap')).toBeNull())
  })
})

describe('Console text safety', () => {
  it('shows markup in a log line literally and never as elements', () => {
    const run = runOf(runIds.partiallySucceeded)
    const evil = '<script>window.__pwned = 1</script><img src=x onerror="window.__pwned = 2">'
    const { container } = render(<Console run={run} live={liveOf(run, [line(0, 1, evil)])} scope={wholeRun} />)
    expect(container.querySelector('script')).toBeNull()
    expect(container.querySelector('img')).toBeNull()
    expect(container.querySelector('.con-tx')?.textContent).toBe(evil)
    expect((window as unknown as { __pwned?: number }).__pwned).toBeUndefined()
  })
})

describe('Console connection and notices', () => {
  const run = runOf(runIds.running)

  it('says nothing about the connection while live', () => {
    render(<Console run={run} live={liveOf(run, [], { connection: 'live' })} scope={wholeRun} />)
    expect(screen.queryByRole('status')).toBeNull()
  })

  it('says the log could not be read completely, with the reason and a retry, instead of "No log lines yet"', async () => {
    const finished = runOf(runIds.succeeded)
    const retry = vi.fn()
    const error = new ApiError(500, 'internal', 'log store down')
    render(<Console run={finished} live={liveOf(finished, [line(0, 1, 'only the first page')], { error, retry })} scope={wholeRun} />)
    expect(screen.getByRole('alert')).toHaveTextContent('The log could not be read completely: log store down')
    await userEvent.click(screen.getByRole('button', { name: 'Retry' }))
    expect(retry).toHaveBeenCalledTimes(1)
  })

  it('does not say "No log lines yet" when reading the log failed with no lines held', () => {
    const finished = runOf(runIds.succeeded)
    const error = new ApiError(500, 'internal', 'log store down')
    render(<Console run={finished} live={liveOf(finished, [], { error })} scope={wholeRun} />)
    expect(screen.queryByText('No log lines yet')).not.toBeInTheDocument()
  })

  it('leaves "live updates stopped" to the run page for a run that is still active', () => {
    const error = new ApiError(403, 'forbidden', 'You may not view this run')
    render(<Console run={run} live={liveOf(run, [], { error })} scope={wholeRun} />)
    expect(screen.queryByText(/Live updates stopped/)).toBeNull()
  })

  it('shows "Reconnecting" while the connection retries', () => {
    render(<Console run={run} live={liveOf(run, [], { connection: 'reconnecting' })} scope={wholeRun} />)
    expect(screen.getByRole('status')).toHaveTextContent('Reconnecting')
  })

  it('tells a person without log access, and makes no log request', async () => {
    function Harness() {
      const live = useRunLive(runIds.running, { logs: false })
      return live.run ? <Console run={live.run} live={live} scope={wholeRun} /> : null
    }
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } })
    render(
      <QueryClientProvider client={client}>
        <Harness />
      </QueryClientProvider>,
    )
    expect(await screen.findByText('You do not have access to logs in this environment')).toBeInTheDocument()
    expect(requests.filter((request) => request.path.endsWith('/log'))).toEqual([])
    expect(screen.queryByRole('link', { name: 'Download' })).toBeNull()
  })

  it('shows a notice with the download link when the oldest lines were dropped', () => {
    const finished = runOf(runIds.partiallySucceeded)
    render(<Console run={finished} live={liveOf(finished, sample, { truncated: true })} scope={wholeRun} />)
    const notice = screen.getByText(/Only the latest 20,000 lines are shown/)
    expect(notice).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Download the full log' })).toBeInTheDocument()
  })
})

describe('Console download', () => {
  const run = runOf(runIds.partiallySucceeded)
  let saved: { blob: Blob; name: string }[]

  beforeEach(() => {
    saved = []
    let blobs = 0
    const made = new Map<string, Blob>()
    URL.createObjectURL = (blob: Blob | MediaSource) => {
      const url = `blob:test/${(blobs += 1)}`
      made.set(url, blob as Blob)
      return url
    }
    URL.revokeObjectURL = () => {}
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      saved.push({ blob: made.get(this.href) as Blob, name: this.download })
    })
  })
  afterEach(() => vi.restoreAllMocks())

  const apiLine = (legId: number, seq: number, text: string, at = '2026-11-27T10:03:00Z'): ApiLogLine => ({ leg_id: legId, seq, at, level: 'info', text })
  const logRequests = () => requests.filter((request) => request.path === '/api/v1/runs/105/log')

  it('reads every page of the scope, in order, and saves one text line per log line with the gap notice', async () => {
    const many = Array.from({ length: 2300 }, (_, index) => apiLine(10504, index + 1, `<b>billing</b> ${index + 1}`, `2026-11-27T10:${String(Math.floor(index / 60)).padStart(2, '0')}:${String(index % 60).padStart(2, '0')}Z`))
    mockState().logs[105] = [...many, apiLine(10503, 1, 'other service')]
    mockState().logGaps[105] = [{ leg_id: 10504, from_seq: 5000, to_seq: 5009 }]
    render(<Console run={run} live={liveOf(run, [])} scope={{ kind: 'service', service: 'billing' }} />)
    await userEvent.click(screen.getByRole('button', { name: 'Download' }))
    await waitFor(() => expect(saved).toHaveLength(1))
    expect(logRequests().map((request) => request.search)).toEqual([
      '?service=billing&limit=1000',
      '?service=billing&limit=1000&after=c1000',
      '?service=billing&limit=1000&after=c2000',
    ])
    expect(saved[0]?.name).toBe('run-105-billing.log')
    expect(saved[0]?.blob.type).toBe('text/plain')
    const text = await readBlob(saved[0]?.blob as Blob)
    const rows = text.trimEnd().split('\n')
    expect(rows).toHaveLength(2301)
    expect(rows[0]).toBe('2026-11-27T10:00:00Z info <b>billing</b> 1')
    expect(rows[2299]).toContain('<b>billing</b> 2300')
    expect(rows[2300]).toBe('-- Lines 5000 to 5009 of this step are missing --')
    expect(text).not.toContain('other service')
  })

  it('shows the alert, not an unhandled rejection, when the file cannot be saved', async () => {
    mockState().logs[105] = [apiLine(10504, 1, 'x')]
    URL.createObjectURL = () => {
      throw new Error('no blobs here')
    }
    render(<Console run={run} live={liveOf(run, [])} scope={wholeRun} />)
    await userEvent.click(screen.getByRole('button', { name: 'Download' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('The log could not be downloaded')
  })

  it('says so when a page fails, and works again on the next try', async () => {
    mockState().logs[105] = [apiLine(10504, 1, 'x')]
    failNext('GET', '/api/v1/runs/105/log', 500, 'internal')
    render(<Console run={run} live={liveOf(run, [])} scope={wholeRun} />)
    await userEvent.click(screen.getByRole('button', { name: 'Download' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('The log could not be downloaded')
    expect(saved).toEqual([])
    // Trying again works and clears the message.
    await userEvent.click(screen.getByRole('button', { name: 'Download' }))
    await waitFor(() => expect(saved).toHaveLength(1))
    expect(screen.queryByRole('alert')).toBeNull()
  })

  it('cuts the planning scopes in the browser, since the server cannot filter by them', async () => {
    mockState().logs[105] = [apiLine(10501, 1, 'resolved'), apiLine(10504, 1, 'copied')]
    render(<Console run={run} live={liveOf(run, [])} scope={{ kind: 'step', step: ':resolve' }} />)
    await userEvent.click(screen.getByRole('button', { name: 'Download' }))
    await waitFor(() => expect(saved).toHaveLength(1))
    expect(logRequests().map((request) => request.search)).toEqual(['?limit=1000'])
    expect(await readBlob(saved[0]?.blob as Blob)).toBe('2026-11-27T10:03:00Z info resolved\n')
    expect(saved[0]?.name).toBe('run-105-resolve.log')
  })
})

function readBlob(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result))
    reader.onerror = () => reject(reader.error)
    reader.readAsText(blob)
  })
}

describe('Console line numbers and planning scopes', () => {
  it('keeps the number of a held line when the oldest lines are dropped, so held rows are not redrawn', () => {
    const run = runOf(runIds.partiallySucceeded)
    const all = Array.from({ length: 8 }, (_, index) => line(0, index + 1, `n${index + 1}`))
    const numberOf = (container: HTMLElement, text: string) =>
      Array.from(container.querySelectorAll('.con-ln')).find((row) => row.querySelector('.con-tx')?.textContent === text)?.querySelector('.con-no')?.textContent
    const { container, rerender } = render(<Console run={run} live={liveOf(run, all.slice(0, 6))} scope={wholeRun} />)
    expect(numberOf(container, 'n3')).toBe('3')
    rerender(<Console run={run} live={liveOf(run, all.slice(2))} scope={wholeRun} />) // n1 and n2 dropped, n7 and n8 added
    expect(numberOf(container, 'n3')).toBe('3')
    expect(numberOf(container, 'n8')).toBe('8')
    expect(container.querySelectorAll('.con-ln')).toHaveLength(6)
  })

  it('shows the planning legs under ":resolve" and keeps a real step named "resolve" apart', () => {
    const base = runOf(runIds.partiallySucceeded)
    const run = structuredClone(base)
    for (const step of run.steps) if (step.name === 'copy-config') step.name = 'resolve'
    for (const leg of run.legs) if (leg.step === 'copy-config') leg.step = 'resolve'
    const lines = [line(10501, 1, 'planning line'), line(10503, 1, 'real step line')]
    const { container, rerender } = render(<Console run={run} live={liveOf(run, lines)} scope={{ kind: 'step', step: ':resolve' }} />)
    expect(rowsOf(container).join('\n')).toContain('planning line')
    expect(rowsOf(container).join('\n')).not.toContain('real step line')
    expect(screen.getByText('Resolve')).toBeInTheDocument()
    rerender(<Console run={run} live={liveOf(run, lines)} scope={{ kind: 'step', step: 'resolve' }} />)
    expect(rowsOf(container).join('\n')).toContain('real step line')
    expect(rowsOf(container).join('\n')).not.toContain('planning line')
  })
})

describe('Console follow tail', () => {
  /** Gives the log body a scroll geometry (jsdom has none) and returns it. */
  function geometry(scrollTop: number): HTMLElement {
    const body = screen.getByRole('log')
    Object.defineProperty(body, 'scrollHeight', { configurable: true, value: 1000 })
    Object.defineProperty(body, 'clientHeight', { configurable: true, value: 200 })
    Object.defineProperty(body, 'scrollTop', { configurable: true, writable: true, value: scrollTop })
    fireEvent.scroll(body)
    return body
  }
  const run = runOf(runIds.running)
  const lines = (count: number) => Array.from({ length: count }, (_, index) => line(0, index + 1, `n${index}`, 'info', 104))

  it('keeps the view at the bottom when new lines arrive and the person is at the bottom', () => {
    const { rerender } = render(<Console run={run} live={liveOf(run, lines(3), { connection: 'live' })} scope={wholeRun} />)
    const body = geometry(800)
    act(() => {
      Object.defineProperty(body, 'scrollHeight', { configurable: true, value: 1200 })
      rerender(<Console run={run} live={liveOf(run, lines(6), { connection: 'live' })} scope={wholeRun} />)
    })
    expect(body.scrollTop).toBe(1200)
    expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull()
  })

  it('stays put when the person scrolled up, and offers "Jump to latest"', async () => {
    const { rerender } = render(<Console run={run} live={liveOf(run, lines(3), { connection: 'live' })} scope={wholeRun} />)
    const body = geometry(100)
    rerender(<Console run={run} live={liveOf(run, lines(6), { connection: 'live' })} scope={wholeRun} />)
    expect(body.scrollTop).toBe(100)
    await userEvent.click(screen.getByRole('button', { name: 'Jump to latest' }))
    expect(body.scrollTop).toBe(1000)
    expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull()
  })

  it('does not offer "Jump to latest" for a finished run', () => {
    const finished = runOf(runIds.partiallySucceeded)
    render(<Console run={finished} live={liveOf(finished, lines(3))} scope={wholeRun} />)
    geometry(0)
    expect(screen.queryByRole('button', { name: 'Jump to latest' })).toBeNull()
  })
})

describe('Console with thousands of lines', () => {
  it('renders 5,000 lines within 15 times the 500-line time, and one click on a failed service shows only its lines', async () => {
    const run = runOf(runIds.partiallySucceeded)
    const legIds = [10503, 10504, 10505, 10506, 10507, 10508]
    const makeLines = (count: number) => {
      const made: LogLine[] = []
      const seqs = new Map<number, number>()
      let billing = 0
      for (let index = 0; index < count; index += 1) {
        const legId = legIds[index % legIds.length] ?? 0
        const seq = (seqs.get(legId) ?? 0) + 1
        seqs.set(legId, seq)
        if (run.legs.find((leg) => leg.id === legId)?.service === 'billing') billing += 1
        made.push(line(legId, seq, `line ${index}`))
      }
      return { made, billing }
    }
    const { made: lines, billing: billingFailed } = makeLines(5000)

    function Harness() {
      const [scope, setScope] = useState<ConsoleScope>(wholeRun)
      return (
        <MemoryRouter>
          <StepsTree run={run} lines={lines} scope={scope} onSelect={setScope} />
          <Console run={run} live={liveOf(run, lines)} scope={scope} />
        </MemoryRouter>
      )
    }
    // CPU time of this process, not wall time: the suite runs on a shared, busy machine where wall time varies several-fold.
    const cpuMs = (since: NodeJS.CpuUsage) => {
      const used = process.cpuUsage(since)
      return (used.user + used.system) / 1000
    }
    // A baseline on this machine, right now: 500 lines. The bound is a multiple of it, so a slow CI box passes
    // and a render that grows worse than linearly does not.
    const small = makeLines(500).made
    const baselineStart = process.cpuUsage()
    const baseline = render(<Console run={run} live={liveOf(run, small)} scope={wholeRun} />)
    const t500 = cpuMs(baselineStart)
    expect(rowsOf(baseline.container)).toHaveLength(500)
    baseline.unmount()

    const start = process.cpuUsage()
    const { container } = render(<Harness />)
    const t5000 = cpuMs(start)
    expect(rowsOf(container)).toHaveLength(5000)
    expect(t5000).toBeLessThanOrEqual(15 * Math.max(t500, 50))

    const clickStart = process.cpuUsage()
    await userEvent.click(screen.getAllByRole('button', { name: /^billing/ })[0] as HTMLElement)
    const shown = rowsOf(container)
    // A hang still fails: the test's own timeout ends it.
    expect(cpuMs(clickStart)).toBeLessThanOrEqual(15 * Math.max(t500, 50))
    expect(shown).toHaveLength(billingFailed)
    expect(billingFailed).toBeGreaterThan(40)
    expect(billingFailed).toBeLessThan(5000)
  }, 15000)
})
