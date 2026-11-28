import { act, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { Plan, RunDetail } from '../../api/types'
import { runIds } from '../../test/fixtures'
import { delayNext, failNext, mockState, requests } from '../../test/server'
import { renderApp } from '../../test/render'
import { PlanPanel } from './PlanPanel'

function runOf(id: number): RunDetail {
  return mockState().runs.find((run) => run.id === id)!
}

async function renderPlan(id: number) {
  const view = renderApp(<PlanPanel run={runOf(id)} />)
  await screen.findByRole('table', { name: /plan/i })
  return view
}

function rowOf(service: string) {
  return screen.getByRole('row', { name: new RegExp(`^${service}\\b`) })
}

describe('PlanPanel', () => {
  it('shows the version change, mutes the old one, and says unchanged when equal', async () => {
    const run = runOf(runIds.planned)
    const plan = mockState().plans[run.id]!
    plan.services[1]!.previous_version = plan.services[1]!.version
    await renderPlan(run.id)
    const changed = rowOf('backend-api')
    expect(within(changed).getByText('1.9.3')).toHaveClass('plan-old')
    expect(within(changed).getByText('1.9.4')).toBeInTheDocument()
    const versionCell = within(rowOf('billing')).getAllByRole('cell')[1]!
    expect(within(versionCell).getByText('unchanged')).toHaveClass('plan-old')
    expect(within(rowOf('billing')).queryByText('1.9.3')).not.toBeInTheDocument()
    expect(within(changed).getByText(plan.services[0]!.previous_task_definition)).toBeInTheDocument()
  })

  it('lists added, removed and changed key names of a changed env file', async () => {
    await renderPlan(runIds.planned)
    const row = rowOf('backend-api')
    expect(within(row).getByText('changed')).toBeInTheDocument()
    expect(within(row).getByText('Added')).toBeInTheDocument()
    expect(within(row).getByText('FEATURE_X_ENABLED')).toBeInTheDocument()
    expect(within(row).getByText('Removed')).toBeInTheDocument()
    expect(within(row).getByText('OLD_TIMEOUT')).toBeInTheDocument()
    expect(within(row).getByText('DB_POOL_SIZE')).toBeInTheDocument()
  })

  it('says only changed or unchanged for a TOML file, never keys', async () => {
    const plan = mockState().plans[runIds.planned]!
    const toml = plan.services[1]!.config[0]!
    toml.change = 'changed'
    toml.keys = { added: ['LEAK_KEY'], removed: [], changed: [] }
    await renderPlan(runIds.planned)
    const row = rowOf('billing')
    expect(within(row).getByText('changed')).toBeInTheDocument()
    expect(within(row).queryByText('LEAK_KEY')).not.toBeInTheDocument()
    expect(within(row).queryByText('Added')).not.toBeInTheDocument()
  })

  it('shows a config file whose change could not be told as Unknown and does not count it as a change', async () => {
    const plan = mockState().plans[runIds.planned]!
    for (const service of plan.services) for (const item of service.config) item.change = 'unchanged'
    plan.services[1]!.config[0]!.change = 'unknown'
    await renderPlan(runIds.planned)
    const row = rowOf('billing')
    expect(within(row).getByText('Unknown')).toBeInTheDocument()
    expect(screen.getByText(/no config files change/)).toBeInTheDocument()
  })

  it('shows a failed check as an error note with the service, detail and "This run cannot start"', async () => {
    // Task 4's RunActions hides Start for a failed check; this test covers the note only.
    await renderPlan(runIds.plannedFailedCheck)
    const note = screen.getByRole('alert')
    expect(note).toHaveTextContent('This run cannot start')
    expect(note).toHaveTextContent('image-exists')
    expect(note).toHaveTextContent('billing')
    expect(note).toHaveTextContent('Image billing:2.5.0 was not found in ECR')
  })

  it('shows no error note when no check failed', async () => {
    await renderPlan(runIds.planned)
    expect(screen.queryByRole('alert')).not.toBeInTheDocument()
    expect(screen.queryByText(/This run cannot start/)).not.toBeInTheDocument()
  })

  it('names the services and the branch when env files came from the fallback branch', async () => {
    await renderPlan(runIds.planned)
    expect(
      screen.getByText('Env files for backend-api come from branch dev because branch v1.9.4 does not exist'),
    ).toBeInTheDocument()
  })

  it('writes one fallback sentence per branch and version, never mixing them', async () => {
    const plan = mockState().plans[runIds.planned]!
    const billing = plan.services[1]!
    billing.version = '2.4.0'
    billing.config[0]!.fallback = true
    billing.config[0]!.branch = 'main'
    await renderPlan(runIds.planned)
    expect(
      screen.getByText('Env files for backend-api come from branch dev because branch v1.9.4 does not exist'),
    ).toBeInTheDocument()
    expect(
      screen.getByText('Env files for billing come from branch main because branch v2.4.0 does not exist'),
    ).toBeInTheDocument()
  })

  it('does not invent a branch name when the plan has none', async () => {
    const plan = mockState().plans[runIds.planned]!
    delete plan.services[0]!.config[0]!.branch
    await renderPlan(runIds.planned)
    expect(screen.getByText(/Env files for backend-api come from another branch because branch v1\.9\.4/)).toBeInTheDocument()
    expect(screen.queryByText(/branch dev/)).not.toBeInTheDocument()
  })

  it('shows a line for checks that could not be verified', async () => {
    await renderPlan(runIds.planned)
    expect(screen.getByText(/could not be verified/i)).toHaveTextContent('config-source-version')
    expect(screen.getByText(/could not be verified/i)).toHaveTextContent('billing')
  })

  it('is open while planned and collapses to a one-line summary afterwards', async () => {
    const { container, unmount } = await renderPlan(runIds.planned)
    expect(container.querySelector('details')).toHaveAttribute('open')
    expect(screen.getByText('Plan: 2 services, 1 config file changes')).toBeInTheDocument()
    unmount()
    const done = renderApp(<PlanPanel run={runOf(runIds.succeeded)} />)
    await screen.findByText(/^Plan: 1 service/)
    expect(done.container.querySelector('details')).not.toHaveAttribute('open')
  })

  it('is open while waiting for approval', async () => {
    const { container } = await renderPlan(runIds.waitingApproval)
    expect(container.querySelector('details')).toHaveAttribute('open')
  })

  it('labels a restore "now" and "restored to", and warns about items not restored', async () => {
    const plan = mockState().plans[runIds.restoreSucceeded]!
    const reason = plan.services.flatMap((s) => s.config).find((item) => item.not_restored)!.not_restored!
    await renderPlan(runIds.restoreSucceeded)
    expect(screen.getByRole('columnheader', { name: 'Now' })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: 'Restored to' })).toBeInTheDocument()
    expect(screen.getByText(reason)).toBeInTheDocument()
    expect(screen.getAllByRole('row')[1]).toHaveTextContent(plan.services[0]!.target_task_definition!)
  })

  it('labels a deploy "From" and "To"', async () => {
    await renderPlan(runIds.planned)
    expect(screen.getByRole('columnheader', { name: 'From' })).toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: 'To' })).toBeInTheDocument()
  })

  it('shows plain text, not an error page, while the run has no plan yet', async () => {
    const run = { ...runOf(runIds.planned), id: 9999 }
    renderApp(<PlanPanel run={run} />)
    await waitFor(() => expect(screen.getByText(/loading plan|not planned yet/i)).toBeInTheDocument())
    expect(screen.queryByText('Something went wrong')).not.toBeInTheDocument()
  })

  it('never renders a value, even one a plan carries in a field the component does not read', async () => {
    const secret = 'hunter2-s3cret-value'
    const plan = mockState().plans[runIds.planned]!
    const item = plan.services[0]!.config[0]! as Plan['services'][number]['config'][number] & Record<string, unknown>
    item.value = secret
    item.content = secret
    ;(item.keys as unknown as Record<string, unknown>).values = { DB_POOL_SIZE: secret }
    ;(plan.services[0]! as unknown as Record<string, unknown>).env_values = { DB_POOL_SIZE: secret }
    await renderPlan(runIds.planned)
    expect(document.body.textContent).not.toContain(secret)
  })
})

describe('PlanPanel after the run has failed its preflight', () => {
  it('shows the failed check note without opening the plan card', async () => {
    const run = runOf(runIds.plannedFailedCheck)
    run.state = 'failed'
    renderApp(<PlanPanel run={run} />)
    expect(await screen.findByText('This run cannot start.')).toBeVisible()
  })

  it('shows the plan when the run left resolving while the first plan request was still in flight', async () => {
    const run = { ...runOf(runIds.planned), state: 'resolving' as const }
    // The first request was answered "not planned yet" a moment before the plan existed; it is slow to arrive.
    delayNext('GET', `/api/v1/runs/${run.id}/plan`, 200)
    failNext('GET', `/api/v1/runs/${run.id}/plan`, 404, 'not_found', 'plan not found')
    const { rerender } = renderApp(<PlanPanel run={run} />)
    await waitFor(() => expect(requests.some((request) => request.path.endsWith('/plan'))).toBe(true))
    // The state event arrives while that request is pending.
    act(() => rerender(<PlanPanel run={{ ...run, state: 'planned' }} />))
    expect(await screen.findByRole('table', { name: /plan/i })).toBeInTheDocument()
  })
})
