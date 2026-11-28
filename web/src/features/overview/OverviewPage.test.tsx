import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Overview } from '../../api/types'
import { overviewFixture } from '../../test/handlers'
import { renderApp } from '../../test/render'
import { delayNext, failNext, requests, server } from '../../test/server'
import { OverviewPage } from './OverviewPage'

const path = '/api/v1/analytics/overview'

function answer(over: Partial<Overview>, days = 14) {
  server.use(http.get(path, () => HttpResponse.json({ ...overviewFixture(days), ...over })))
}

const zeroRuns: Partial<Overview> = {
  runs: 0,
  prev_runs: 0,
  success_rate: null,
  prev_success_rate: null,
  median_duration_seconds: null,
  prev_median_duration_seconds: null,
  waiting_approval: 0,
  oldest_waiting: null,
  days: overviewFixture(14).days.map((day) => ({ ...day, succeeded: 0, failed: 0, other: 0 })),
  top_failing: [],
  attention: [],
  environments: [],
}

function tile(label: string) {
  return screen.getByText(label).closest('.ui-tile') as HTMLElement
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'], now: new Date('2026-11-28T12:00:00Z') })
})
afterEach(() => {
  vi.useRealTimers()
})

describe('the overview tiles', () => {
  it('compares each number with the previous period', async () => {
    answer({ runs: 24, prev_runs: 6, success_rate: 0.8, prev_success_rate: 0.83, median_duration_seconds: 134, prev_median_duration_seconds: 150 })
    renderApp(<OverviewPage />)
    await screen.findByText('+18 vs the previous 14 days')
    expect(within(tile('Runs')).getByText('24')).toBeInTheDocument()
    expect(within(tile('Success rate')).getByText('80%')).toBeInTheDocument()
    expect(within(tile('Success rate')).getByText('3 points lower than the previous 14 days')).toBeInTheDocument()
    expect(within(tile('Median duration')).getByText('2m 14s')).toBeInTheDocument()
    expect(within(tile('Median duration')).getByText('16s faster than the previous 14 days')).toBeInTheDocument()
    expect(within(tile('Waiting for approval')).getByText('Oldest requested 2 hours ago')).toBeInTheDocument()
  })

  it('words the comparison the other way when the numbers went the other way', async () => {
    answer({ runs: 4, prev_runs: 10, success_rate: 0.9, prev_success_rate: 0.8, median_duration_seconds: 200, prev_median_duration_seconds: 150, waiting_approval: 0, oldest_waiting: null })
    renderApp(<OverviewPage />)
    await screen.findByText('6 fewer than the previous 14 days')
    expect(screen.getByText('10 points higher than the previous 14 days')).toBeInTheDocument()
    expect(screen.getByText('50s slower than the previous 14 days')).toBeInTheDocument()
    expect(screen.getByText('Nothing is waiting')).toBeInTheDocument()
  })

  it('says the number is the same when nothing changed', async () => {
    answer({ runs: 10, prev_runs: 10, success_rate: 0.8, prev_success_rate: 0.8, median_duration_seconds: 60, prev_median_duration_seconds: 60 })
    renderApp(<OverviewPage />)
    await screen.findAllByText('Same as the previous 14 days')
    expect(screen.getAllByText('Same as the previous 14 days')).toHaveLength(3)
  })

  it('says so when the previous period had no finished runs', async () => {
    answer({ prev_success_rate: null, prev_median_duration_seconds: null })
    renderApp(<OverviewPage />)
    expect(await screen.findAllByText('No finished runs in the previous 14 days')).not.toHaveLength(0)
  })

  it('names the period it was asked for in the comparisons', async () => {
    answer({ runs: 5, prev_runs: 2 }, 7)
    renderApp(<OverviewPage />, { route: '/?days=7' })
    expect(await screen.findByText('+3 vs the previous 7 days')).toBeInTheDocument()
  })
})

describe('a period with no runs', () => {
  it('shows empty states and never a made-up number', async () => {
    answer(zeroRuns)
    const { container } = renderApp(<OverviewPage />)
    await screen.findByText('Nothing needs attention')
    expect(within(tile('Runs')).getByText('No runs in this period')).toBeInTheDocument()
    expect(within(tile('Success rate')).getByText('No runs in this period')).toBeInTheDocument()
    expect(within(tile('Median duration')).getByText('No runs in this period')).toBeInTheDocument()
    expect(within(tile('Waiting for approval')).getByText('Nothing is waiting')).toBeInTheDocument()
    expect(screen.getByText('No failed deployments in this period')).toBeInTheDocument()
    const text = container.textContent ?? ''
    for (const bad of ['NaN', 'Infinity', 'undefined', 'null', '0%']) {
      expect(text).not.toContain(bad)
    }
  })
})

describe('the overview content', () => {
  it('shows runs per day with a readout and a table for assistive technology', async () => {
    renderApp(<OverviewPage />)
    expect(await screen.findByRole('table', { name: 'Runs per day' })).toBeInTheDocument()
    const first = overviewFixture(14).days[0]!
    expect(screen.getByRole('button', { name: new RegExp(`^${Number(first.date.slice(8))} November 2026: `) })).toBeInTheDocument()
  })

  it('lists the services that failed most and links each to its page', async () => {
    renderApp(<OverviewPage />)
    expect(await screen.findByRole('link', { name: 'billing-api' })).toHaveAttribute('href', '/services/billing-api')
    expect(screen.getByText('3 failures')).toBeInTheDocument()
    expect(screen.getByText('Health check failed')).toBeInTheDocument()
  })

  it('links each item that needs attention to its run', async () => {
    renderApp(<OverviewPage />)
    const link = await screen.findByRole('link', { name: /deploy-billing/ })
    expect(link).toHaveAttribute('href', '/runs/41')
    expect(link).toHaveTextContent('billing-api did not become healthy')
    expect(link).toHaveTextContent('Failed')
  })

  it('shows the last run per environment, or that there is none yet', async () => {
    renderApp(<OverviewPage />)
    const table = await screen.findByRole('table', { name: 'Environments' })
    const rows = within(table).getAllByRole('row')
    expect(within(rows[1]!).getByRole('link', { name: '#41' })).toHaveAttribute('href', '/runs/41')
    expect(within(rows[2]!).getByText('No runs yet')).toBeInTheDocument()
  })
})

describe('the filters', () => {
  it('starts with all environments and 14 days, without putting them in the address', async () => {
    const { location } = renderApp(<OverviewPage />)
    await screen.findByRole('table', { name: 'Environments' })
    expect(requests.find((r) => r.path === path)?.search).toBe('?days=14')
    expect(location().search).toBe('')
    expect(screen.getByRole('button', { name: 'All environments' })).toHaveAttribute('aria-pressed', 'true')
  })

  it('offers one option per label in the inventory and sends the choice in the request and the URL', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    const { location } = renderApp(<OverviewPage />)
    const group = await screen.findByRole('group', { name: 'Environment label' })
    await waitFor(() => expect(within(group).getAllByRole('button')).toHaveLength(4))
    expect(within(group).getAllByRole('button').map((b) => b.textContent)).toEqual(['All environments', 'dev', 'preprod', 'prod'])
    await user.click(within(group).getByRole('button', { name: 'prod' }))
    await waitFor(() => expect(requests.some((r) => r.path === path && r.search.includes('label=prod'))).toBe(true))
    expect(location().search).toContain('label=prod')
  })

  it('sends the period in the request and the URL', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    const { location } = renderApp(<OverviewPage />)
    await user.selectOptions(await screen.findByLabelText('Period'), '30')
    await waitFor(() => expect(requests.some((r) => r.path === path && r.search.includes('days=30'))).toBe(true))
    expect(location().search).toContain('days=30')
  })

  it('reads both filters from the address', async () => {
    renderApp(<OverviewPage />, { route: '/?label=preprod&days=7' })
    await screen.findByRole('table', { name: 'Environments' })
    const seen = requests.find((r) => r.path === path)!
    expect(seen.search).toContain('label=preprod')
    expect(seen.search).toContain('days=7')
    expect(screen.getByLabelText('Period')).toHaveValue('7')
  })
})

describe('changing the filters while the answer is on its way', () => {
  it('comparison text matches the data shown while a new period loads', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    const { container } = renderApp(<OverviewPage />)
    await screen.findByText(/vs the previous 14 days/)
    delayNext('GET', path, 300)
    await user.selectOptions(screen.getByLabelText('Period'), '30')
    // The 14-day numbers are still on screen: they must not claim to be compared with 30 days.
    expect(screen.queryByText(/previous 30 days/)).not.toBeInTheDocument()
    expect(screen.getByText(/vs the previous 14 days/)).toBeInTheDocument()
    expect(container.querySelector('[aria-busy="true"]')).not.toBeNull()
    expect(await screen.findByText(/vs the previous 30 days/, undefined, { timeout: 2000 })).toBeInTheDocument()
    expect(container.querySelector('[aria-busy="true"]')).toBeNull()
  })
})

describe('loading and failing', () => {
  it('shows a loading state first', async () => {
    delayNext('GET', path, 50)
    renderApp(<OverviewPage />)
    expect(screen.getByRole('status')).toBeInTheDocument()
    await screen.findByRole('table', { name: 'Environments' }, { timeout: 2000 })
  })

  it('shows the error with a retry that loads the page', async () => {
    const user = userEvent.setup({ advanceTimers: vi.advanceTimersByTime })
    failNext('GET', path, 500, 'internal', 'The overview could not be read')
    renderApp(<OverviewPage />)
    expect(await screen.findByRole('alert')).toHaveTextContent('The overview could not be read')
    await user.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('table', { name: 'Environments' })).toBeInTheDocument()
  })
})
