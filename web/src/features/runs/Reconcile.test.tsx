import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { Me, RunDetail } from '../../api/types'
import { renderApp } from '../../test/render'
import { runIds } from '../../test/fixtures'
import { delayNext, failNext, mockState, requests, viewerMe } from '../../test/server'
import { Reconcile } from './Reconcile'

function unknownRun(): RunDetail {
  const run = mockState().runs.find((candidate) => candidate.id === runIds.needsReconciliation)
  if (!run) throw new Error('fixture run 106 is missing')
  return structuredClone(run)
}

const restorer: Me = { ...viewerMe, id: 20, name: 'Gal Grantee', grants: [{ action: 'restore', label: '', environment: '', folder_id: 0 }] }

async function renderPanel(run: RunDetail, me: Me | undefined, onShowLog = vi.fn()) {
  const view = renderApp(<Reconcile run={run} onShowLog={onShowLog} />, { me })
  await waitFor(() => expect(screen.queryByText("Loading your session")).not.toBeInTheDocument())
  return { ...view, onShowLog }
}

function rowOf(service: string) {
  return screen.getByRole('row', { name: new RegExp(service) })
}

describe('Reconcile', () => {
  it('shows a row per unknown leg with what was intended and why it is unknown', async () => {
    await renderPanel(unknownRun(), undefined)
    expect(screen.getByText(/could not confirm what happened/i)).toBeInTheDocument()
    const backend = rowOf('backend-api')
    expect(within(backend).getByText('deploy')).toBeInTheDocument()
    expect(within(backend).getByText('Update service to backend-api:1.9.3')).toBeInTheDocument()
    expect(within(backend).getByText(/runner stopped answering/)).toBeInTheDocument()
    expect(within(rowOf('billing')).getByText('Update service to billing:2.3.9')).toBeInTheDocument()
    // Legs that are not unknown get no row.
    expect(screen.getAllByRole('row')).toHaveLength(3)
  })

  it('opens the log of the leg service', async () => {
    const { onShowLog } = await renderPanel(unknownRun(), undefined)
    await userEvent.click(within(rowOf('billing')).getByRole('button', { name: 'Show its log' }))
    expect(onShowLog).toHaveBeenCalledWith({ kind: 'service', service: 'billing' })
  })

  it('checks a leg again without asking', async () => {
    await renderPanel(unknownRun(), undefined)
    await userEvent.click(within(rowOf('billing')).getByRole('button', { name: 'Check again' }))
    await waitFor(() => expect(requests.at(-1)?.path).toBe('/api/v1/runs/106/legs/10606/check-again'))
    expect(requests.at(-1)?.method).toBe('POST')
  })

  it.each([
    ['Mark succeeded', 'succeeded'],
    ['Mark failed', 'failed'],
  ])('%s asks first, naming the service, then posts the outcome and closes', async (button, outcome) => {
    await renderPanel(unknownRun(), undefined)
    await userEvent.click(within(rowOf('billing')).getByRole('button', { name: button }))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText(new RegExp(`billing.*${outcome}`, 'i'))).toBeInTheDocument()
    expect(requests.filter((request) => request.path.endsWith('/reconcile'))).toHaveLength(0)
    await userEvent.click(within(dialog).getByRole('button', { name: button }))
    await waitFor(() => expect(requests.at(-1)?.path).toBe('/api/v1/runs/106/legs/10606/reconcile'))
    expect(requests.at(-1)?.body).toEqual({ outcome })
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('disables the Mark buttons of a leg while its request is pending', async () => {
    delayNext('POST', '/api/v1/runs/106/legs/10606/reconcile', 300)
    await renderPanel(unknownRun(), undefined)
    await userEvent.click(within(rowOf('billing')).getByRole('button', { name: 'Mark succeeded' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Mark succeeded' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(within(rowOf('billing')).getByRole('button', { name: 'Mark succeeded' })).toBeDisabled()
    expect(within(rowOf('billing')).getByRole('button', { name: 'Mark failed' })).toBeDisabled()
    await waitFor(() => expect(requests.filter((request) => request.path.endsWith('/reconcile'))).toHaveLength(1))
    // Let the delayed answer arrive before the test ends: a request still waiting in the mock would otherwise
    // wake during a later test and take the failure that test queued for the same path.
    await waitFor(() => expect(within(rowOf('billing')).getByRole('button', { name: 'Mark succeeded' })).toBeEnabled())
  })

  it('posts nothing when the confirmation is dismissed', async () => {
    await renderPanel(unknownRun(), undefined)
    await userEvent.click(within(rowOf('billing')).getByRole('button', { name: 'Mark failed' }))
    await userEvent.click(screen.getByRole('button', { name: 'Go back' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(requests.filter((request) => request.method === 'POST')).toHaveLength(0)
  })

  it('shows the message of a 409 from the server and closes the dialog', async () => {
    failNext('POST', '/api/v1/runs/106/legs/10606/reconcile', 409, 'conflict', 'run is not in a state that allows this: it is succeeded')
    await renderPanel(unknownRun(), undefined)
    await userEvent.click(within(rowOf('billing')).getByRole('button', { name: 'Mark succeeded' }))
    await userEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Mark succeeded' }))
    expect(await screen.findByRole('alert')).toHaveTextContent('run is not in a state that allows this: it is succeeded')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
  })

  it('has no action buttons without restore, but still offers the log', async () => {
    await renderPanel(unknownRun(), viewerMe)
    const row = rowOf('billing')
    expect(within(row).getByRole('button', { name: 'Show its log' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /check again|mark succeeded|mark failed/i })).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /release locks/i })).not.toBeInTheDocument()
  })

  it('gives a person with restore the three buttons but not Release locks', async () => {
    await renderPanel(unknownRun(), restorer)
    expect(within(rowOf('billing')).getByRole('button', { name: 'Mark failed' })).toBeInTheDocument()
    expect(screen.queryByRole('button', { name: /release locks/i })).not.toBeInTheDocument()
  })

  it('lets an admin release the locks after a confirmation that says what follows', async () => {
    await renderPanel(unknownRun(), undefined)
    await userEvent.click(screen.getByRole('button', { name: 'Release locks without resolving' }))
    const dialog = screen.getByRole('dialog')
    expect(within(dialog).getByText(/marked failed/i)).toBeInTheDocument()
    expect(within(dialog).getByText(/other runs may then touch these services/i)).toBeInTheDocument()
    await userEvent.click(within(dialog).getByRole('button', { name: 'Release locks' }))
    await waitFor(() => expect(requests.at(-1)?.path).toBe('/api/v1/admin/runs/106/release-locks'))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
  })

  it('disappears when the next state event leaves no unknown leg, and stays while one remains', async () => {
    const run = unknownRun()
    const { rerender, container } = await renderPanel(run, undefined)
    const oneResolved: RunDetail = { ...run, legs: run.legs.map((leg) => (leg.id === 10605 ? { ...leg, state: 'succeeded' } : leg)) }
    act(() => rerender(<Reconcile run={oneResolved} onShowLog={vi.fn()} />))
    expect(screen.queryByRole('row', { name: /backend-api/ })).not.toBeInTheDocument()
    expect(rowOf('billing')).toBeInTheDocument()
    const allResolved: RunDetail = { ...run, state: 'partially_succeeded', legs: oneResolved.legs.map((leg) => (leg.id === 10606 ? { ...leg, state: 'failed' } : leg)) }
    act(() => rerender(<Reconcile run={allResolved} onShowLog={vi.fn()} />))
    expect(container).toBeEmptyDOMElement()
  })

  it('renders nothing when the run is not waiting for reconciliation', async () => {
    const { container } = await renderPanel({ ...unknownRun(), state: 'running' }, undefined)
    expect(container).toBeEmptyDOMElement()
  })
})
