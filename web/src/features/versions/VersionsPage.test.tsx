import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { RunningMatrix } from '../../api/types'
import { absentCell, runningCell } from '../../test/handlers'
import { renderApp } from '../../test/render'
import { connectionsManagerMe, failNext, server, viewerMe } from '../../test/server'
import { VersionsPage } from './VersionsPage'

const path = '/api/v1/versions/running'

// Two dev environments and one prod, so "same label" comparisons have something to compare.
const environments = [
  { name: 'hetzner-01', label: 'dev', aws_account: '111111111111' },
  { name: 'hetzner-02', label: 'dev', aws_account: '111111111111' },
  { name: 'prod-01', label: 'prod', aws_account: '333333333333' },
]

const matrix: RunningMatrix = {
  environments,
  rows: [
    // Same version everywhere: no difference.
    { service: 'alpha', cells: [runningCell('1.0.0'), runningCell('1.0.0'), runningCell('1.0.0')] },
    // Two dev environments disagree, and the older one is marked behind.
    { service: 'bravo', cells: [runningCell('1.0.0', { behind: true }), runningCell('1.1.0'), runningCell('1.0.0')] },
    // One environment could not be read; the others still show versions.
    {
      service: 'charlie',
      cells: [runningCell('3.0.0'), { ...absentCell, state: 'error', error: 'AccessDenied' }, runningCell('3.0.0')],
    },
    // Deployed outside Yumdrake, version unknown, not set up in the second dev environment.
    { service: 'delta', cells: [runningCell('', { by_yumdrake: false, run_id: 0 }), absentCell, runningCell('4.0.0', { by_yumdrake: false, run_id: 0 })] },
    { service: 'echo', cells: [runningCell('5.0.0', { last_deploy_failed: true }), runningCell('5.0.0'), runningCell('5.0.0')] },
  ],
}

function answer(body: RunningMatrix = matrix) {
  server.use(http.get(path, () => HttpResponse.json(body)))
}

function rowOf(service: string): HTMLElement {
  return screen.getByRole('link', { name: service }).closest('tr') as HTMLElement
}

const serviceLinks = () => screen.getAllByRole('link', { name: /^(alpha|bravo|charlie|delta|echo|foxtrot)$/ })

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'], now: new Date('2026-11-28T12:00:00Z') })
  answer()
})
afterEach(() => {
  vi.useRealTimers()
})

describe('the matrix', () => {
  it('has a column per environment with its label, and a row per service linking to its page', async () => {
    renderApp(<VersionsPage />)
    const table = await screen.findByRole('table', { name: 'Running versions' })
    const headers = within(table).getAllByRole('columnheader').map((header) => header.textContent)
    expect(headers).toEqual(['Service', 'hetzner-01dev', 'hetzner-02dev', 'prod-01prod'])
    expect(screen.getByRole('link', { name: 'alpha' })).toHaveAttribute('href', '/services/alpha')
    expect(serviceLinks()).toHaveLength(5)
  })

  it('shows the version in mono with the task definition and read time as its tooltip', async () => {
    renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    const version = within(rowOf('alpha')).getAllByText('1.0.0')[0]!
    expect(version).toHaveClass('mono')
    expect(version.closest('[title]')).toHaveAttribute('title', 'billing-api:100 · read 2 minutes ago')
  })

  it('says "unknown version" when ECS runs a service but no version was found', async () => {
    renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    expect(within(rowOf('delta')).getByText('unknown version')).toBeInTheDocument()
  })

  it('shows "not set up" for an absent cell', async () => {
    renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    expect(within(rowOf('delta')).getByText('not set up')).toBeInTheDocument()
  })

  it('shows "could not read" with the AWS code for one environment while the others show their versions', async () => {
    renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    const row = within(rowOf('charlie'))
    expect(row.getByText('could not read')).toBeInTheDocument()
    expect(row.getByText('AccessDenied')).toHaveClass('mono')
    expect(row.getAllByText('3.0.0')).toHaveLength(2)
    // The other services are not touched by it.
    expect(within(rowOf('alpha')).getAllByText('1.0.0')).toHaveLength(3)
  })
})

describe('the markers', () => {
  it('marks a cell that is behind another environment with the same label, in words', async () => {
    renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    const marked = within(rowOf('bravo')).getAllByText('behind')
    expect(marked).toHaveLength(1)
    expect(marked[0]!.closest('td')).toHaveTextContent('1.0.0')
  })

  it('marks a failed last deploy', async () => {
    renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    expect(within(rowOf('echo')).getAllByText('last deploy failed')).toHaveLength(1)
  })

  it('marks what Yumdrake did not deploy, and nothing for what it did', async () => {
    renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    expect(within(rowOf('delta')).getAllByText('not deployed by Yumdrake')).toHaveLength(2)
    expect(within(rowOf('alpha')).queryByText('not deployed by Yumdrake')).not.toBeInTheDocument()
  })

  it('explains the three markers in a legend', async () => {
    renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    const legend = screen.getByRole('list', { name: 'Markers' })
    expect(within(legend).getByText('Behind another environment with the same label')).toBeInTheDocument()
    expect(within(legend).getByText('Last deploy failed')).toBeInTheDocument()
    expect(within(legend).getByText('Not deployed by Yumdrake')).toBeInTheDocument()
  })
})

describe('the first column stays while the rest scrolls', () => {
  it('gives the service header and every service cell the sticky class', async () => {
    renderApp(<VersionsPage />)
    const table = await screen.findByRole('table', { name: 'Running versions' })
    const firstCells = within(table)
      .getAllByRole('row')
      .map((row) => row.firstElementChild as HTMLElement)
    expect(firstCells).toHaveLength(6)
    for (const cell of firstCells) expect(cell).toHaveClass('versions-service-col')
    // The other columns are not sticky.
    expect(within(table).getAllByRole('columnheader')[1]).not.toHaveClass('versions-service-col')
  })

  it('scrolls the table inside its card, not the page', async () => {
    renderApp(<VersionsPage />)
    const table = await screen.findByRole('table', { name: 'Running versions' })
    const wrapper = table.closest('.versions-scroll')
    expect(wrapper).not.toBeNull()
    expect(wrapper!.closest('.ui-card')).not.toBeNull()
  })
})

describe('search and differences only', () => {
  it('filters rows by service name without caring about case, keeps it in the address and counts', async () => {
    const user = userEvent.setup()
    const { location } = renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    expect(screen.getByText('5 of 5 services')).toBeInTheDocument()
    await user.type(screen.getByRole('searchbox', { name: 'Find a service' }), 'ALP')
    expect(screen.queryByRole('link', { name: 'bravo' })).not.toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'alpha' })).toBeInTheDocument()
    expect(screen.getByText('1 of 5 services')).toBeInTheDocument()
    expect(location().search).toBe('?q=ALP')
  })

  it('says when no service matches', async () => {
    renderApp(<VersionsPage />, { route: '/versions?q=zzz' })
    expect(await screen.findByText('No service matches “zzz”')).toBeInTheDocument()
    expect(screen.getByText('0 of 5 services')).toBeInTheDocument()
  })

  it('keeps only rows that differ, are behind, failed or not deployed by Yumdrake', async () => {
    const user = userEvent.setup()
    const { location } = renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    await user.click(screen.getByRole('checkbox', { name: 'Only differences' }))
    // alpha is the same everywhere; charlie only has a read error, which is not a difference of versions.
    expect(serviceLinks().map((link) => link.textContent)).toEqual(['bravo', 'delta', 'echo'])
    expect(screen.getByText('3 of 5 services')).toBeInTheDocument()
    expect(location().search).toBe('?diff=1')
  })

  it('compares versions only between environments with the same label', async () => {
    answer({
      environments,
      rows: [{ service: 'foxtrot', cells: [runningCell('1.0.0'), runningCell('1.0.0'), runningCell('9.9.9')] }],
    })
    renderApp(<VersionsPage />, { route: '/versions?diff=1' })
    expect(await screen.findByText('0 of 1 services')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'foxtrot' })).not.toBeInTheDocument()
  })

  it('reads both from the address', async () => {
    renderApp(<VersionsPage />, { route: '/versions?q=a&diff=1' })
    await screen.findByRole('table', { name: 'Running versions' })
    expect(screen.getByRole('checkbox', { name: 'Only differences' })).toBeChecked()
    expect(screen.getByRole('searchbox', { name: 'Find a service' })).toHaveValue('a')
  })
})

describe('the row detail', () => {
  it('opens per row from a button, for keyboard and touch, and lists every environment', async () => {
    const user = userEvent.setup()
    renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    const button = within(rowOf('alpha')).getByRole('button', { name: 'Show details for alpha' })
    expect(button).toHaveAttribute('aria-expanded', 'false')
    await user.click(button)
    expect(button).toHaveAttribute('aria-expanded', 'true')
    const detail = screen.getByRole('region', { name: 'alpha in each environment' })
    expect(within(detail).getAllByText('billing-api:100')).toHaveLength(3)
    expect(within(detail).getAllByText('read 2 minutes ago')).toHaveLength(3)
    expect(within(detail).getAllByRole('link', { name: '#12' })[0]).toHaveAttribute('href', '/runs/12')
    await user.click(button)
    expect(screen.queryByRole('region', { name: 'alpha in each environment' })).not.toBeInTheDocument()
  })

  it('spells out markers, errors and absent environments in words', async () => {
    const user = userEvent.setup()
    renderApp(<VersionsPage />)
    await screen.findByRole('table', { name: 'Running versions' })
    await user.click(screen.getByRole('button', { name: 'Show details for delta' }))
    const delta = screen.getByRole('region', { name: 'delta in each environment' })
    expect(within(delta).getAllByText('Not deployed by Yumdrake')).toHaveLength(2)
    expect(within(delta).getByText('Not set up')).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Show details for charlie' }))
    expect(within(screen.getByRole('region', { name: 'charlie in each environment' })).getByText('Could not read: AccessDenied')).toBeInTheDocument()
  })
})

describe('reading again', () => {
  it('is offered to people who manage connections, asks for a fresh read and replaces the data', async () => {
    const user = userEvent.setup()
    const asked: string[] = []
    server.use(
      http.get(path, ({ request }) => {
        asked.push(new URL(request.url).search)
        const fresh = new URL(request.url).searchParams.get('refresh') === '1'
        return HttpResponse.json(fresh ? { environments, rows: [{ service: 'newest', cells: [runningCell('7.0.0'), absentCell, absentCell] }] } : matrix)
      }),
    )
    renderApp(<VersionsPage />, { me: connectionsManagerMe })
    await screen.findByRole('link', { name: 'alpha' })
    await user.click(screen.getByRole('button', { name: 'Read again' }))
    expect(await screen.findByRole('link', { name: 'newest' })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'alpha' })).not.toBeInTheDocument()
    expect(asked).toEqual(['', '?refresh=1'])
  })

  it('is not offered to anyone else', async () => {
    renderApp(<VersionsPage />, { me: viewerMe })
    await screen.findByRole('link', { name: 'alpha' })
    expect(screen.queryByRole('button', { name: 'Read again' })).not.toBeInTheDocument()
  })
})

describe('empty and failing', () => {
  it('says so when the inventory has no services', async () => {
    answer({ environments, rows: [] })
    renderApp(<VersionsPage />)
    expect(await screen.findByText('No services in the inventory')).toBeInTheDocument()
  })

  it('shows the error with a retry', async () => {
    const user = userEvent.setup()
    server.resetHandlers() // back to the shared mock, which the injected failure belongs to
    failNext('GET', path, 500, 'internal', 'The versions could not be read')
    renderApp(<VersionsPage />)
    expect(await screen.findByRole('alert')).toHaveTextContent('The versions could not be read')
    await user.click(screen.getByRole('button', { name: 'Try again' }))
    await waitFor(() => expect(screen.getByRole('link', { name: 'backend-api' })).toBeInTheDocument())
  })
})
