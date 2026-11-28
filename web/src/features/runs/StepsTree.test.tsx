import { render as renderBare, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import type { ReactElement } from 'react'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it, vi } from 'vitest'
import type { LogLine, RunDetail } from '../../api/types'
import { mockState, runIds } from '../../test/server'
import { StepsTree } from './StepsTree'
import { wholeRun } from './scope'

// The tree links service names to their pages, so it needs a router.
const render = (ui: ReactElement) => renderBare(<MemoryRouter>{ui}</MemoryRouter>)

const runOf =(id: number): RunDetail => {
  const run = mockState().runs.find((candidate) => candidate.id === id)
  if (!run) throw new Error('no such run')
  return run
}

const lineOf = (legId: number, seq: number): LogLine => ({ runId: 105, legId, seq, at: '2026-11-27T10:03:00Z', level: 'info', text: `t${seq}` })

describe('StepsTree', () => {
  it('lists the whole run first with its line count, then planning legs, then the steps in order', () => {
    const lines = [lineOf(0, 1), lineOf(10503, 1), lineOf(10504, 1)]
    render(<StepsTree run={runOf(runIds.partiallySucceeded)} lines={lines} scope={wholeRun} onSelect={() => {}} />)
    const names = screen.getAllByRole('button').map((button) => button.textContent ?? '')
    expect(names[0]).toMatch(/^Whole run\s*3$/)
    const order = ['Resolve', 'Revalidate', 'copy-config', 'deploy', 'verify']
    const positions = order.map((name) => names.findIndex((text) => text.startsWith(name)))
    expect(positions.every((position) => position > 0)).toBe(true)
    expect([...positions].sort((a, b) => a - b)).toEqual(positions)
  })

  it('puts a failed service above the others in its step, and shows a count of done services as the step meta', () => {
    render(<StepsTree run={runOf(runIds.partiallySucceeded)} lines={[]} scope={wholeRun} onSelect={() => {}} />)
    const copyConfig = screen.getByRole('button', { name: /^copy-config/ })
    expect(copyConfig).toHaveTextContent('1/2')
    const buttons = screen.getAllByRole('button')
    const start = buttons.indexOf(copyConfig)
    // The two services of copy-config follow it: billing (failed) first, backend-api second.
    expect(buttons[start + 1]).toHaveTextContent(/^billing/)
    expect(buttons[start + 2]).toHaveTextContent(/^backend-api/)
    expect(buttons[start + 1]).toHaveTextContent('Failed')
  })

  it('links each service to its page, next to its button and without changing the button', () => {
    render(<StepsTree run={runOf(runIds.partiallySucceeded)} lines={[]} scope={wholeRun} onSelect={() => {}} />)
    const link = screen.getAllByRole('link', { name: 'billing service page' })[0]!
    expect(link).toHaveAttribute('href', '/services/billing')
    expect(link.closest('button')).toBeNull()
  })

  it('puts an unknown service above the others too', () => {
    render(<StepsTree run={runOf(runIds.needsReconciliation)} lines={[]} scope={wholeRun} onSelect={() => {}} />)
    const deploy = screen.getByRole('button', { name: /^deploy/ })
    const buttons = screen.getAllByRole('button')
    expect(buttons[buttons.indexOf(deploy) + 1]).toHaveTextContent('Unknown')
  })

  it('reports the chosen item to onSelect: whole run, a step, a service', async () => {
    const onSelect = vi.fn()
    render(<StepsTree run={runOf(runIds.partiallySucceeded)} lines={[]} scope={{ kind: 'step', step: 'deploy' }} onSelect={onSelect} />)
    await userEvent.click(screen.getByRole('button', { name: /^verify/ }))
    expect(onSelect).toHaveBeenLastCalledWith({ kind: 'step', step: 'verify' })
    await userEvent.click(screen.getAllByRole('button', { name: /^billing/ })[0] as HTMLElement)
    expect(onSelect).toHaveBeenLastCalledWith({ kind: 'service', service: 'billing' })
    await userEvent.click(screen.getByRole('button', { name: /^Whole run/ }))
    expect(onSelect).toHaveBeenLastCalledWith({ kind: 'run' })
  })

  it('marks the selected item with aria-current', () => {
    render(<StepsTree run={runOf(runIds.partiallySucceeded)} lines={[]} scope={{ kind: 'step', step: 'deploy' }} onSelect={() => {}} />)
    const current = screen.getAllByRole('button').filter((button) => button.getAttribute('aria-current') === 'true')
    expect(current).toHaveLength(1)
    expect(current[0]).toHaveTextContent(/^deploy/)
  })

  it('shows "attempt 2" for a service that ran twice, and nothing for a first attempt', () => {
    const run = structuredClone(runOf(runIds.partiallySucceeded))
    const retried = run.legs.find((leg) => leg.id === 10504)
    if (!retried) throw new Error('missing leg')
    retried.attempt = 2
    render(<StepsTree run={run} lines={[]} scope={wholeRun} onSelect={() => {}} />)
    const tree = screen.getByRole('navigation', { name: 'Steps and services' })
    expect(within(tree).getAllByText('attempt 2')).toHaveLength(2) // the service and its step
    expect(within(tree).queryByText('attempt 1')).toBeNull()
  })
})

describe('StepsTree planning items and the current item', () => {
  it('keeps the planning items apart from a real step named "resolve"', async () => {
    const run = structuredClone(runOf(runIds.partiallySucceeded))
    for (const step of run.steps) if (step.name === 'copy-config') step.name = 'resolve'
    for (const leg of run.legs) if (leg.step === 'copy-config') leg.step = 'resolve'
    const onSelect = vi.fn()
    render(<StepsTree run={run} lines={[]} scope={{ kind: 'step', step: 'resolve' }} onSelect={onSelect} />)
    const current = screen.getAllByRole('button').filter((button) => button.getAttribute('aria-current') === 'true')
    expect(current).toHaveLength(1)
    expect(current[0]).toHaveTextContent(/^resolve1\/2/)
    await userEvent.click(screen.getByRole('button', { name: /^Resolve/ }))
    expect(onSelect).toHaveBeenLastCalledWith({ kind: 'step', step: ':resolve' })
    await userEvent.click(screen.getByRole('button', { name: /^resolve/ }))
    expect(onSelect).toHaveBeenLastCalledWith({ kind: 'step', step: 'resolve' })
  })

  it('marks only one row as current when the selected service is listed under several steps', () => {
    render(<StepsTree run={runOf(runIds.partiallySucceeded)} lines={[]} scope={{ kind: 'service', service: 'billing' }} onSelect={() => {}} />)
    const current = screen.getAllByRole('button').filter((button) => button.getAttribute('aria-current') === 'true')
    expect(current).toHaveLength(1)
    expect(current[0]).toHaveTextContent(/^billing/)
  })
})
