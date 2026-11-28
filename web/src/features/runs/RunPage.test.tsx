import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { adminMe, eventSources, failNext, mockState, qaMe, requests, runIds, viewerMe } from '../../test/server'
import { actionNames, moveStoredRun, renderRun, storedRun } from './runTestUtils'

const lead = () => screen.findByTestId('run-lead')

describe('the run page header', () => {
  it('shows the crumbs, the title with its status and the facts row', async () => {
    renderRun(runIds.partiallySucceeded)
    expect(await screen.findByRole('heading', { level: 1, name: /Run #105/ })).toHaveTextContent('Partly succeeded')
    const crumbs = screen.getByRole('navigation', { name: 'Breadcrumb' })
    expect(within(crumbs).getByRole('link', { name: 'Jobs' })).toHaveAttribute('href', '/jobs')
    const facts = screen.getByText('Environment').closest('dl') as HTMLElement
    expect(within(facts).getByText('hetzner-02')).toBeInTheDocument()
    expect(within(facts).getByText('Dana Levi')).toBeInTheDocument()
    expect(within(facts).getByText('via web')).toBeInTheDocument()
    expect(within(facts).getByText('7m 30s')).toBeInTheDocument()
    expect(within(facts).getByRole('img', { name: /services/ })).toBeInTheDocument()
  })

  it('says "service" in the singular for a run of one service', async () => {
    const run = storedRun(runIds.partiallySucceeded)
    const failedService = run.legs.find((leg) => leg.state === 'failed')?.service
    run.legs = run.legs.filter((leg) => leg.service === failedService)
    renderRun(runIds.partiallySucceeded)
    expect(await lead()).toHaveTextContent(/^0 of 1 service updated$/)
    const facts = screen.getByText('Environment').closest('dl') as HTMLElement
    expect(within(facts).getByText('1 service, 0 updated')).toBeInTheDocument()
  })

  it('says the run cannot be found for an unknown id', async () => {
    renderRun(999)
    expect(await screen.findByText(/Run not found|not found/i)).toBeInTheDocument()
  })
})

describe('the sentence that says where the run stands', () => {
  it('planned', async () => {
    renderRun(runIds.planned)
    expect(await lead()).toHaveTextContent('Review the plan, then start')
  })

  it('resolving', async () => {
    moveStoredRun(runIds.planned, 'resolving')
    renderRun(runIds.planned)
    expect(await lead()).toHaveTextContent('Planning: reading current versions and config')
  })

  it('waiting for approval, with the expiry', async () => {
    renderRun(runIds.waitingApproval)
    expect(await lead()).toHaveTextContent(/Waiting for approval, expires/)
  })

  it('queued behind another run, with a link to it', async () => {
    moveStoredRun(runIds.running, 'queued', { wait_reason: 'Waiting for run #105 to finish' })
    renderRun(runIds.running)
    const link = await within(await lead()).findByRole('link', { name: 'run #105' })
    expect(link).toHaveAttribute('href', '/runs/105')
    expect(await lead()).toHaveTextContent('Waiting for run #105 to finish')
  })

  it('queued behind a freeze, in the server words', async () => {
    moveStoredRun(runIds.running, 'queued', { wait_reason: 'Frozen: Quarter-end change freeze, until 30 Nov' })
    renderRun(runIds.running)
    expect(await lead()).toHaveTextContent('Frozen: Quarter-end change freeze, until 30 Nov')
  })

  it('running counts services updated so far', async () => {
    renderRun(runIds.running)
    expect(await lead()).toHaveTextContent('0 of 2 services updated so far')
  })

  it('cancelling', async () => {
    moveStoredRun(runIds.running, 'cancelling', { cancel_requested: true })
    renderRun(runIds.running)
    expect(await lead()).toHaveTextContent('Cancelling: waiting for services already rolling to finish')
  })

  it('needs reconciliation counts the unconfirmed outcomes', async () => {
    renderRun(runIds.needsReconciliation)
    expect(await lead()).toHaveTextContent('2 outcomes could not be confirmed. Its services stay locked until they are resolved')
  })

  it('succeeded', async () => {
    renderRun(runIds.succeeded)
    expect(await lead()).toHaveTextContent(/^All \d+ services? updated$/)
  })

  it('partially succeeded', async () => {
    renderRun(runIds.partiallySucceeded)
    expect(await lead()).toHaveTextContent('1 of 2 services updated')
  })

  it('cancelled with the engine default reason says no more than the state, plus how much was updated', async () => {
    moveStoredRun(runIds.partiallySucceeded, 'cancelled', { end_reason: "cancelled at a person's request" })
    renderRun(runIds.partiallySucceeded)
    expect((await lead()).textContent).toBe('Cancelled. 1 service had already been updated')
  })

  it('cancelled names who when the server sends it, and keeps a more specific reason', async () => {
    moveStoredRun(runIds.partiallySucceeded, 'cancelled', { end_reason: 'the deploy window closed', cancelled_by_name: 'Dana Levi' })
    renderRun(runIds.partiallySucceeded)
    expect((await lead()).textContent).toBe('Cancelled by Dana Levi. The deploy window closed. 1 service had already been updated')
  })

  it('invalidated gives the detail', async () => {
    renderRun(runIds.invalidated)
    expect(await lead()).toHaveTextContent('The targets changed after this plan was made. Billing changed after the plan was made')
  })

  it('invalidated with the engine default reason shows the fixed sentence only', async () => {
    moveStoredRun(runIds.invalidated, 'invalidated', { end_reason: 'the targets changed since the plan was made, so nothing was run' })
    renderRun(runIds.invalidated)
    expect((await lead()).textContent).toBe('The targets changed after this plan was made')
  })

  it('rejected says who and when, from the engine reason', async () => {
    moveStoredRun(runIds.waitingApproval, 'rejected', { end_reason: 'rejected by Dana Levi', state_changed_at: '2026-11-27T11:55:00Z' })
    renderRun(runIds.waitingApproval)
    expect(await lead()).toHaveTextContent(/^Rejected by Dana Levi .+ ago$/)
  })

  it('expired says why and when, from the engine reason', async () => {
    moveStoredRun(runIds.planned, 'expired', { end_reason: 'nobody started the plan within 1h0m0s', state_changed_at: '2026-11-27T11:55:00Z' })
    renderRun(runIds.planned)
    expect(await lead()).toHaveTextContent(/^Expired: nobody started the plan within 1h0m0s/)
  })
})

describe('what failed', () => {
  it('lists the failed item, its step and reason, and a line for the skipped steps', async () => {
    renderRun(runIds.partiallySucceeded)
    const panel = await screen.findByRole('region', { name: 'What failed' })
    const rows = within(panel).getAllByRole('row')
    expect(rows).toHaveLength(2)
    expect(within(rows[1]!).getByText('billing')).toBeInTheDocument()
    expect(within(rows[1]!).getByText('copy-config')).toBeInTheDocument()
    expect(within(rows[1]!).getByText(/does not exist \(NoSuchKey\)/)).toBeInTheDocument()
    expect(within(panel).getByText(/: deploy and verify were skipped/).textContent).toBe('billing: deploy and verify were skipped')
  })

  it('links the failed service and the service named in the skipped line to its page', async () => {
    renderRun(runIds.partiallySucceeded)
    const panel = await screen.findByRole('region', { name: 'What failed' })
    const links = within(panel).getAllByRole('link', { name: 'billing' })
    expect(links).toHaveLength(2)
    for (const link of links) expect(link).toHaveAttribute('href', '/services/billing')
    expect(within(panel).getByText(/: deploy and verify were skipped/)).toBeInTheDocument()
  })

  it('"Show its log" puts the service in the address, which is the console scope', async () => {
    const view = renderRun(runIds.partiallySucceeded)
    await userEvent.click(await screen.findByRole('button', { name: 'Show its log' }))
    expect(view.location().search).toBe('?service=billing')
  })

  it('is not shown for a cancelled run with no failure and nothing skipped', async () => {
    const run = moveStoredRun(runIds.succeeded, 'cancelled', { end_reason: "cancelled at a person's request" })
    run.summary.failures = []
    renderRun(runIds.succeeded)
    await lead()
    expect(screen.queryByRole('region', { name: 'What failed' })).not.toBeInTheDocument()
  })

  it('is not shown for a run that succeeded', async () => {
    renderRun(runIds.succeeded)
    await lead()
    expect(screen.queryByRole('region', { name: 'What failed' })).not.toBeInTheDocument()
  })
})

describe('watching a run change under the page', () => {
  it('swaps the buttons when a state event moves 103 from waiting approval to running', async () => {
    renderRun(runIds.waitingApproval)
    await waitFor(async () => expect(await actionNames()).toEqual(['Approve', 'Reject', 'Cancel']))
    const run = storedRun(runIds.waitingApproval)
    await waitFor(() => expect(eventSources.all.length).toBeGreaterThan(0))
    act(() => {
      eventSources.last()?.open()
      eventSources.last()?.emit('state', {
        id: run.id,
        state: 'running',
        wait_reason: '',
        end_reason: '',
        state_changed_at: '2026-11-27T12:01:00Z',
        started_at: '2026-11-27T12:01:00Z',
        finished_at: null,
        cancel_requested: false,
        legs: run.legs,
        locks: run.locks ?? [],
        summary: run.summary,
      })
    })
    await waitFor(async () => expect(await actionNames()).toEqual(['Cancel']))
  })
})

describe('who can see logs', () => {
  it('makes no log request for a person who may not view logs', async () => {
    renderRun(runIds.succeeded, viewerMe)
    await lead()
    await waitFor(() => expect(requests.some((request) => request.path === `/api/v1/runs/${runIds.succeeded}`)).toBe(true))
    expect(requests.filter((request) => request.path.endsWith('/log'))).toEqual([])
  })

  it('reads the log for an admin', async () => {
    renderRun(runIds.succeeded, adminMe)
    await lead()
    await waitFor(() => expect(requests.some((request) => request.path.endsWith('/log'))).toBe(true))
  })

  it('does not read a prod run log for a person with logs in dev only', async () => {
    renderRun(runIds.waitingApproval, qaMe)
    await lead()
    expect(requests.filter((request) => request.path.endsWith('/log'))).toEqual([])
  })
})

describe('when the log or the live updates fail', () => {
  it('says the log could not be read in the console, never "No log lines yet", and Retry reads it again', async () => {
    failNext('GET', `/api/v1/runs/${runIds.succeeded}/log`, 500, 'internal', 'log store down')
    renderRun(runIds.succeeded, adminMe)
    const consolePanel = await screen.findByRole('region', { name: 'Console' })
    expect(await within(consolePanel).findByRole('alert')).toHaveTextContent('The log could not be read completely: log store down')
    expect(within(consolePanel).queryByText('No log lines yet')).not.toBeInTheDocument()
    await userEvent.click(within(consolePanel).getByRole('button', { name: 'Retry' }))
    await waitFor(() => expect(within(consolePanel).queryByRole('alert')).not.toBeInTheDocument())
    expect(consolePanel.querySelectorAll('.con-ln').length).toBeGreaterThan(0)
  })

  it('stops saying the run is live when the stream is refused later, and labels the state as the last known', async () => {
    renderRun(runIds.running, adminMe)
    await waitFor(() => expect(eventSources.all.length).toBeGreaterThan(0))
    const stream = eventSources.last()!
    act(() => stream.open())
    failNext('GET', `/api/v1/runs/${runIds.running}`, 403, 'forbidden', 'You may not view this run')
    act(() => stream.fail())
    const note = await screen.findByTestId('live-stopped', undefined, { timeout: 5000 })
    expect(note).toHaveTextContent('Live updates stopped: You may not view this run')
    expect(note).toHaveTextContent('last known state')
    expect(screen.getAllByText(/Live updates stopped/)).toHaveLength(1)
    expect(screen.getByTestId('run-lead')).toBeInTheDocument()
  })
})

describe('the plan of a run that is still being planned', () => {
  it('appears on its own once the run leaves resolving, for a person who cannot start it', async () => {
    const plan = mockState().plans[runIds.planned]!
    delete mockState().plans[runIds.planned]
    moveStoredRun(runIds.planned, 'resolving')
    renderRun(runIds.planned, viewerMe)
    expect(await screen.findByText(/Loading plan/)).toBeInTheDocument()
    mockState().plans[runIds.planned] = plan
    const run = moveStoredRun(runIds.planned, 'planned')
    await waitFor(() => expect(eventSources.all.length).toBeGreaterThan(0))
    act(() => {
      eventSources.last()?.open()
      eventSources.last()?.emit('state', {
        id: run.id,
        state: 'planned',
        wait_reason: '',
        end_reason: '',
        state_changed_at: '2026-11-27T12:01:00Z',
        started_at: null,
        finished_at: null,
        cancel_requested: false,
        legs: run.legs,
        locks: run.locks ?? [],
        summary: run.summary,
      })
    })
    expect(await screen.findByRole('table', { name: 'Plan' })).toBeInTheDocument()
  })

  it('says the run has no plan once it is final and none was made', async () => {
    delete mockState().plans[runIds.succeeded]
    renderRun(runIds.succeeded, viewerMe)
    expect(await screen.findByText('This run has no plan.')).toBeInTheDocument()
    expect(screen.queryByText(/Loading plan/)).not.toBeInTheDocument()
  })
})
