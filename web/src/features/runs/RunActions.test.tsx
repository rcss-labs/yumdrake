import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import { adminMe, delayNext, eventSources, failNext, jobManagerMe, mockState, qaMe, requests, runIds, server, viewerMe } from '../../test/server'
import type { ApprovalItem, Me } from '../../api/types'
import { actionNames, moveStoredRun, renderRun, storedRun } from './runTestUtils'

const grantee = (id: number, ...actions: Me['grants'][number]['action'][]): Me => ({
  ...viewerMe,
  id,
  name: `Grantee ${id}`,
  grants: actions.map((action) => ({ action, label: '', environment: '', folder_id: 0 })),
})

const posts = (suffix: string) => requests.filter((request) => request.method === 'POST' && request.path.endsWith(suffix))

describe('which buttons each state shows an admin', () => {
  const cases: { name: string; id: number; prepare?: () => void; buttons: string[] }[] = [
    { name: 'resolving', id: runIds.planned, prepare: () => moveStoredRun(runIds.planned, 'resolving'), buttons: [] },
    { name: 'planned', id: runIds.planned, buttons: ['Start', 'Discard'] },
    { name: 'waiting_approval', id: runIds.waitingApproval, buttons: ['Approve', 'Reject', 'Cancel'] },
    { name: 'queued', id: runIds.running, prepare: () => moveStoredRun(runIds.running, 'queued'), buttons: ['Cancel'] },
    { name: 'running', id: runIds.running, buttons: ['Cancel'] },
    { name: 'cancelling', id: runIds.running, prepare: () => moveStoredRun(runIds.running, 'cancelling'), buttons: [] },
    { name: 'needs_reconciliation', id: runIds.needsReconciliation, buttons: [] },
    { name: 'succeeded', id: runIds.succeeded, buttons: ['Restore'] },
    { name: 'partially_succeeded', id: runIds.partiallySucceeded, buttons: ['Re-run failed', 'Restore'] },
    { name: 'failed', id: runIds.partiallySucceeded, prepare: () => moveStoredRun(runIds.partiallySucceeded, 'failed'), buttons: ['Re-run failed', 'Restore'] },
    { name: 'cancelled', id: runIds.partiallySucceeded, prepare: () => moveStoredRun(runIds.partiallySucceeded, 'cancelled'), buttons: ['Re-run failed', 'Restore'] },
    { name: 'invalidated', id: runIds.invalidated, buttons: ['Plan again'] },
    { name: 'rejected', id: runIds.waitingApproval, prepare: () => moveStoredRun(runIds.waitingApproval, 'rejected'), buttons: ['Plan again'] },
    { name: 'expired', id: runIds.waitingApproval, prepare: () => moveStoredRun(runIds.waitingApproval, 'expired'), buttons: ['Plan again'] },
  ]

  it.each(cases)('$name', async ({ id, prepare, buttons }) => {
    prepare?.()
    renderRun(id, adminMe)
    await screen.findByTestId('run-lead')
    if (buttons.length === 0) {
      await waitFor(() => expect(screen.queryByRole('group', { name: 'Run actions' })).not.toBeInTheDocument())
    } else {
      // Approve and Reject wait for the approvals list, so the bar may fill in after it first shows.
      await waitFor(async () => expect(await actionNames()).toEqual(buttons))
    }
  })

  it('hides Restore when nothing was updated', async () => {
    const run = moveStoredRun(runIds.partiallySucceeded, 'failed')
    for (const leg of run.legs) if (leg.kind === 'step') leg.state = leg.state === 'succeeded' ? 'failed' : leg.state
    renderRun(runIds.partiallySucceeded, adminMe)
    expect(await actionNames()).toEqual(['Re-run failed'])
  })
})

describe('a person who may only view', () => {
  it.each([
    ['planned', runIds.planned],
    ['waiting approval', runIds.waitingApproval],
    ['running', runIds.running],
    ['partly succeeded', runIds.partiallySucceeded],
    ['succeeded', runIds.succeeded],
    ['invalidated', runIds.invalidated],
  ])('sees no buttons on a %s run', async (_name, id) => {
    renderRun(id, viewerMe)
    await screen.findByTestId('run-lead')
    expect(screen.queryByRole('group', { name: 'Run actions' })).not.toBeInTheDocument()
    expect(within(document.body).queryAllByRole('button', { name: /^(Start|Discard|Cancel|Approve|Reject|Restore|Re-run failed|Plan again)$/ })).toEqual([])
  })

  it('does not ask for the approvals list, and shows no reason', async () => {
    renderRun(runIds.waitingApproval, viewerMe)
    await screen.findByTestId('run-lead')
    expect(requests.some((request) => request.path === '/api/v1/approvals')).toBe(false)
    expect(screen.queryByText(/You do not have permission to approve/)).not.toBeInTheDocument()
  })
})

describe('the approvals row decides Approve and Reject', () => {
  const row = (extra: Partial<ApprovalItem>): ApprovalItem => ({ run_id: runIds.waitingApproval, job_id: 'x', environment: 'prod-01', requested_by_name: 'Yael Barak', requested_at: '', expires_at: '', versions: [], can_approve: true, reason: '', ...extra })
  let answered = false
  const answerWith = (rows: ApprovalItem[]) => {
    answered = false
    server.use(
      http.get('/api/v1/approvals', () => {
        answered = true
        return HttpResponse.json({ approvals: rows })
      }),
    )
  }

  it('shows Approve and Reject when the row for this run says the viewer can approve', async () => {
    answerWith([row({ can_approve: true })])
    renderRun(runIds.waitingApproval, adminMe)
    await waitFor(async () => expect(await actionNames()).toEqual(['Approve', 'Reject', 'Cancel']))
  })

  it('shows the reason and no Approve or Reject when the row says the viewer cannot', async () => {
    answerWith([row({ can_approve: false, reason: 'You asked for this run.' })])
    renderRun(runIds.waitingApproval, adminMe)
    expect(await screen.findByText('You asked for this run.')).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Reject' })).not.toBeInTheDocument()
  })

  it('shows no Approve or Reject when no row is for this run', async () => {
    answerWith([row({ run_id: 999 })])
    renderRun(runIds.waitingApproval, adminMe)
    await waitFor(() => expect(answered).toBe(true))
    expect(await actionNames()).toEqual(['Cancel'])
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Reject' })).not.toBeInTheDocument()
  })
})

describe('permissions pick the buttons', () => {
  it('Discard needs cancel, not run', async () => {
    renderRun(runIds.planned, grantee(21, 'run'))
    expect(await actionNames()).toEqual(['Start'])
  })

  it('Discard shows with cancel alone', async () => {
    renderRun(runIds.planned, grantee(22, 'cancel'))
    expect(await actionNames()).toEqual(['Discard'])
  })

  it('Restore needs restore', async () => {
    renderRun(runIds.succeeded, grantee(23, 'run'))
    await screen.findByTestId('run-lead')
    expect(screen.queryByRole('button', { name: 'Restore' })).not.toBeInTheDocument()
  })

  it('Start of a restore needs restore, not run', async () => {
    moveStoredRun(runIds.planned, 'planned', { kind: 'restore' })
    renderRun(runIds.planned, grantee(24, 'run'))
    await screen.findByTestId('run-lead')
    await waitFor(() => expect(requests.find((request) => request.path.endsWith('/plan'))?.status).toBe(200))
    expect(screen.queryByRole('button', { name: 'Start' })).not.toBeInTheDocument()
  })

  it('Start of a restore shows with restore alone', async () => {
    moveStoredRun(runIds.planned, 'planned', { kind: 'restore' })
    renderRun(runIds.planned, grantee(25, 'restore'))
    expect(await actionNames()).toEqual(['Start'])
  })

  it('the requester sees the reason instead of Approve and Reject', async () => {
    renderRun(runIds.waitingApproval, { ...jobManagerMe, grants: [...jobManagerMe.grants, { action: 'approve', label: '', environment: '', folder_id: 0 }] })
    expect(await screen.findByText('You started this run, so someone else has to approve it.')).toBeInTheDocument()
    expect(screen.queryByRole('group', { name: 'Run actions' })).not.toBeInTheDocument()
  })
})

describe('Start', () => {
  it('starts a dev run at once with the manifest id', async () => {
    renderRun(runIds.planned, adminMe)
    await screen.findByRole('button', { name: 'Start' })
    await userEvent.click(screen.getByRole('button', { name: 'Start' }))
    await waitFor(() => expect(posts('/start')).toHaveLength(1))
    expect(posts('/start')[0]!.body).toEqual({ manifest_id: storedRun(runIds.planned).manifest_id })
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('asks first in prod, then posts the manifest id', async () => {
    const run = moveStoredRun(runIds.waitingApproval, 'planned', { approval: null })
    run.label = 'prod'
    renderRun(run.id, adminMe)
    await userEvent.click(await screen.findByRole('button', { name: 'Start' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByRole('heading', { name: 'Start in prod-01?' })).toBeInTheDocument()
    expect(posts('/start')).toHaveLength(0)
    await userEvent.click(within(dialog).getByRole('button', { name: 'Start in prod' }))
    await waitFor(() => expect(posts('/start')).toHaveLength(1))
    expect(posts('/start')[0]!.body).toEqual({ manifest_id: run.manifest_id })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('is hidden while the plan has a failed check', async () => {
    renderRun(runIds.plannedFailedCheck, adminMe)
    await screen.findByTestId('run-lead')
    await waitFor(() => expect(requests.some((request) => request.path === `/api/v1/runs/${runIds.plannedFailedCheck}/plan`)).toBe(true))
    await waitFor(() => expect(requests.find((request) => request.path.endsWith('/plan'))?.status).toBe(200))
    expect(screen.queryByRole('button', { name: 'Start' })).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Discard' })).toBeInTheDocument()
  })

  it('is hidden until the plan has loaded', async () => {
    const { delayNext } = await import('../../test/server')
    delayNext('GET', `/api/v1/runs/${runIds.planned}/plan`, 300)
    renderRun(runIds.planned, adminMe)
    await screen.findByTestId('run-lead')
    expect(screen.queryByRole('button', { name: 'Start' })).not.toBeInTheDocument()
    expect(await screen.findByRole('button', { name: 'Start' })).toBeInTheDocument()
  })

  it('a person with run in dev may start but not discard (Discard needs cancel)', async () => {
    for (const check of mockState().plans[runIds.plannedFailedCheck]!.checks) check.status = 'passed'
    renderRun(runIds.plannedFailedCheck, qaMe)
    expect(await actionNames()).toEqual(['Start'])
    expect(screen.queryByRole('button', { name: 'Discard' })).not.toBeInTheDocument()
  })

  it('says why Start is missing when the plan cannot be loaded, and Try again loads it', async () => {
    failNext('GET', `/api/v1/runs/${runIds.planned}/plan`, 500, 'internal', 'plan store down')
    renderRun(runIds.planned, adminMe)
    expect(await screen.findByText(/Start is hidden until the plan loads/)).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: 'Start' })).not.toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('button', { name: 'Start' })).toBeInTheDocument()
  })

  it('a restore-only person sees the plan-failed notice on a restore', async () => {
    moveStoredRun(runIds.planned, 'planned', { kind: 'restore' })
    failNext('GET', `/api/v1/runs/${runIds.planned}/plan`, 500, 'internal', 'plan store down')
    renderRun(runIds.planned, grantee(26, 'restore'))
    expect(await screen.findByText(/Start is hidden until the plan loads/)).toBeInTheDocument()
  })
})

describe('Cancel and Discard', () => {
  it('Cancel explains what will and will not happen, then cancels', async () => {
    renderRun(runIds.running, adminMe)
    await userEvent.click(await screen.findByRole('button', { name: 'Cancel' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Steps that have not started will not run. Services already rolling will finish. Nothing is rolled back.')).toBeInTheDocument()
    expect(posts('/cancel')).toHaveLength(0)
    await userEvent.click(within(dialog).getByRole('button', { name: 'Cancel run' }))
    await waitFor(() => expect(posts('/cancel')).toHaveLength(1))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(posts('/cancel')[0]!.path).toBe(`/api/v1/runs/${runIds.running}/cancel`)
  })

  it('Go back leaves the run alone', async () => {
    renderRun(runIds.running, adminMe)
    await userEvent.click(await screen.findByRole('button', { name: 'Cancel' }))
    await userEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Go back' }))
    expect(posts('/cancel')).toHaveLength(0)
  })

  it('Discard asks, then posts to cancel', async () => {
    renderRun(runIds.planned, adminMe)
    await userEvent.click(await screen.findByRole('button', { name: 'Discard' }))
    const dialog = await screen.findByRole('dialog')
    await userEvent.click(within(dialog).getByRole('button', { name: 'Discard plan' }))
    await waitFor(() => expect(posts('/cancel')).toHaveLength(1))
    expect(posts('/cancel')[0]!.path).toBe(`/api/v1/runs/${runIds.planned}/cancel`)
  })
})

describe('Approve and Reject', () => {
  it('Approve posts the manifest id the page shows and the buttons go away', async () => {
    renderRun(runIds.waitingApproval, adminMe)
    await userEvent.click(await screen.findByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(posts('/approve')).toHaveLength(1))
    expect(posts('/approve')[0]!.body).toEqual({ manifest_id: storedRun(runIds.waitingApproval).manifest_id })
    await waitFor(async () => expect(await actionNames()).toEqual(['Cancel']))
    expect(screen.getByTestId('run-lead')).toHaveTextContent('Waiting for its turn')
  })

  it('Approve and Reject stay disabled while the plan loads, and Approve sends the id of the plan shown', async () => {
    mockState().plans[runIds.waitingApproval]!.id = 'shown-plan-id'
    delayNext('GET', `/api/v1/runs/${runIds.waitingApproval}/plan`, 300)
    renderRun(runIds.waitingApproval, adminMe)
    const approve = await screen.findByRole('button', { name: 'Approve' })
    expect(approve).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Reject' })).toBeDisabled()
    await waitFor(() => expect(approve).toBeEnabled())
    await userEvent.click(approve)
    await waitFor(() => expect(posts('/approve')).toHaveLength(1))
    expect(posts('/approve')[0]!.body).toEqual({ manifest_id: 'shown-plan-id' })
  })

  it('Approve and Reject stay disabled when the plan could not be loaded, and say why', async () => {
    failNext('GET', `/api/v1/runs/${runIds.waitingApproval}/plan`, 500, 'internal', 'plan store down')
    failNext('GET', `/api/v1/runs/${runIds.waitingApproval}/plan`, 500, 'internal', 'plan store down')
    renderRun(runIds.waitingApproval, adminMe)
    const approve = await screen.findByRole('button', { name: 'Approve' })
    expect(await screen.findByText(/Approve and Reject wait for the plan/)).toBeInTheDocument()
    expect(approve).toBeDisabled()
    expect(screen.getByRole('button', { name: 'Reject' })).toBeDisabled()
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    await waitFor(() => expect(screen.getByRole('button', { name: 'Approve' })).toBeEnabled())
  })

  it('Reject asks first', async () => {
    renderRun(runIds.waitingApproval, adminMe)
    await userEvent.click(await screen.findByRole('button', { name: 'Reject' }))
    const dialog = await screen.findByRole('dialog')
    expect(posts('/reject')).toHaveLength(0)
    await userEvent.click(within(dialog).getByRole('button', { name: 'Reject run' }))
    await waitFor(() => expect(posts('/reject')).toHaveLength(1))
    await waitFor(async () => expect(await actionNames()).toEqual(['Plan again']))
  })

  it('a 409 reloads the run and shows the server words, not an error page', async () => {
    renderRun(runIds.waitingApproval, adminMe)
    await screen.findByRole('button', { name: 'Approve' })
    // Someone else rejected it meanwhile.
    moveStoredRun(runIds.waitingApproval, 'rejected', { end_reason: 'Rejected by Yael Barak' })
    failNext('POST', `/api/v1/runs/${runIds.waitingApproval}/approve`, 409, 'conflict', 'This run was already decided.')
    await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
    expect(await screen.findByText('This run was already decided.')).toBeInTheDocument()
    await waitFor(async () => expect(await actionNames()).toEqual(['Plan again']))
    expect(screen.getByRole('heading', { level: 1 })).toHaveTextContent('Rejected')
  })
})

describe('the message after a refused action', () => {
  it('goes away when a later state event changes the state again', async () => {
    renderRun(runIds.waitingApproval, adminMe)
    await screen.findByRole('button', { name: 'Approve' })
    moveStoredRun(runIds.waitingApproval, 'rejected', { end_reason: 'rejected by Yael Barak' })
    failNext('POST', `/api/v1/runs/${runIds.waitingApproval}/approve`, 409, 'conflict', 'This run was already decided.')
    await userEvent.click(screen.getByRole('button', { name: 'Approve' }))
    expect(await screen.findByText('This run was already decided.')).toBeInTheDocument()
    await waitFor(async () => expect(await actionNames()).toEqual(['Plan again']))
    const run = storedRun(runIds.waitingApproval)
    await waitFor(() => expect(eventSources.all.length).toBeGreaterThan(0))
    act(() => {
      eventSources.last()?.open()
      eventSources.last()?.emit('state', {
        id: run.id,
        state: 'queued',
        wait_reason: 'Waiting for its turn',
        end_reason: '',
        state_changed_at: '2026-11-27T12:02:00Z',
        started_at: null,
        finished_at: null,
        cancel_requested: false,
        legs: run.legs,
        locks: [],
        summary: run.summary,
      })
    })
    await waitFor(() => expect(screen.queryByText('This run was already decided.')).not.toBeInTheDocument())
  })
})

describe('Re-run failed', () => {
  it('posts and goes to the new run', async () => {
    const view = renderRun(runIds.partiallySucceeded, adminMe)
    await userEvent.click(await screen.findByRole('button', { name: 'Re-run failed' }))
    await waitFor(() => expect(posts('/rerun-failed')).toHaveLength(1))
    expect(posts('/rerun-failed')[0]!.body).toEqual({})
    await waitFor(() => expect(view.location().pathname).toBe('/runs/110'))
    expect(await screen.findByRole('heading', { level: 1, name: /Run #110/ })).toBeInTheDocument()
  })
})

describe('Plan again', () => {
  it('posts the stored selection, not started, and goes to the new run', async () => {
    const view = renderRun(runIds.invalidated, adminMe)
    await userEvent.click(await screen.findByRole('button', { name: 'Plan again' }))
    const run = storedRun(runIds.invalidated)
    await waitFor(() => expect(requests.some((request) => request.method === 'POST' && request.path === `/api/v1/jobs/${run.job_source_id}/runs`)).toBe(true))
    const request = requests.find((seen) => seen.method === 'POST' && seen.path === `/api/v1/jobs/${run.job_source_id}/runs`)!
    expect(request.body).toEqual({ environment: run.environment, selection: run.selection, start: false })
    await waitFor(() => expect(view.location().pathname).toBe('/runs/110'))
  })

  it('rebuilds the selection from the step legs when the run has none', async () => {
    const run = moveStoredRun(runIds.invalidated, 'invalidated')
    delete run.selection
    const view = renderRun(runIds.invalidated, adminMe)
    await userEvent.click(await screen.findByRole('button', { name: 'Plan again' }))
    await waitFor(() => expect(view.location().pathname).toBe('/runs/110'))
    const request = requests.find((seen) => seen.method === 'POST' && seen.path === `/api/v1/jobs/${run.job_source_id}/runs`)!
    const body = request.body as { selection: Record<string, { version: string; services?: Record<string, string> }> }
    const groups = Object.values(body.selection)
    expect(groups.length).toBeGreaterThan(0)
    expect(groups[0]!.services).toEqual({ billing: expect.any(String) })
  })
})

describe('Restore', () => {
  it('lists the services that go back, then creates the restore run and goes to it', async () => {
    const view = renderRun(runIds.succeeded, adminMe)
    await userEvent.click(await screen.findByRole('button', { name: 'Restore' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/backend-api/)).toBeInTheDocument()
    expect(posts('/restore')).toHaveLength(0)
    await userEvent.click(within(dialog).getByRole('button', { name: 'Plan the restore' }))
    await waitFor(() => expect(posts('/restore')).toHaveLength(1))
    await waitFor(() => expect(view.location().pathname).toBe('/runs/110'))
    expect(await screen.findByTestId('run-lead')).toBeInTheDocument()
  })
})
