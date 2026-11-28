import { screen, within } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { RunSummary } from '../../api/types'
import { mockState, runIds, runRow } from '../../test/server'
import { RunsTable } from './RunsTable'

function rowFor(id: number): RunSummary {
  const run = mockState().runs.find((candidate) => candidate.id === id)
  if (!run) throw new Error(`no run ${id}`)
  return runRow(run)
}

function renderTable(runs: RunSummary[], showJob = true) {
  return render(
    <MemoryRouter>
      <RunsTable runs={runs} showJob={showJob} />
    </MemoryRouter>,
  )
}

const rowOf = (id: number) => screen.getByRole('link', { name: `#${id}` }).closest('tr') as HTMLElement

describe('RunsTable', () => {
  it('links each run and shows its status, job and environment with its label', () => {
    renderTable([rowFor(runIds.partiallySucceeded)])
    const row = rowOf(runIds.partiallySucceeded)
    expect(within(row).getByRole('link', { name: '#105' })).toHaveAttribute('href', '/runs/105')
    expect(within(row).getByText('Partly succeeded')).toBeInTheDocument()
    expect(within(row).getByRole('link', { name: 'Portal services' })).toHaveAttribute('href', '/jobs/3')
    expect(within(row).getByText('hetzner-02')).toBeInTheDocument()
    expect(within(row).getByText('dev')).toBeInTheDocument()
  })

  it('draws one strip cell per service with its status and a title naming service, version and state', () => {
    const run = rowFor(runIds.partiallySucceeded)
    renderTable([run])
    const strip = within(rowOf(105)).getByRole('img')
    const cells = Array.from(strip.querySelectorAll('.ui-strip-cell'))
    expect(cells).toHaveLength(run.services!.length)
    run.services!.forEach((chip, index) => {
      expect(cells[index]).toHaveAttribute('data-status', chip.state)
      expect(cells[index]!.getAttribute('title')).toContain(chip.service)
      expect(cells[index]!.getAttribute('title')).toContain(chip.version)
    })
    expect(within(rowOf(105)).getByText(/^\d+\/\d+$/)).toBeInTheDocument()
  })

  it('lists the short reasons in What failed, and leaves it empty for a clean run', () => {
    renderTable([rowFor(runIds.partiallySucceeded), rowFor(runIds.succeeded)])
    expect(within(rowOf(105)).getByText(/Source file not found/)).toBeInTheDocument()
    expect(within(rowOf(105)).getByText(/billing/, { selector: '.runs-fail *' })).toBeInTheDocument()
    expect(within(rowOf(109)).queryByText(/not found/i)).not.toBeInTheDocument()
    expect(rowOf(109).querySelector('.runs-fail')).toBeEmptyDOMElement()
  })

  it('marks a restore run with the run it restores', () => {
    renderTable([rowFor(runIds.restoreSucceeded)])
    expect(within(rowOf(108)).getByText('Restore of #109')).toBeInTheDocument()
  })

  it('shows who started the run, when and how, and the duration', () => {
    renderTable([rowFor(runIds.succeeded)])
    const row = rowOf(109)
    expect(within(row).getByText('Dana Levi')).toBeInTheDocument()
    expect(within(row).getByText(/ago/)).toBeInTheDocument()
    expect(within(row).getByText('5m 0s')).toBeInTheDocument()
  })

  it('omits the Job column on a job history', () => {
    renderTable([rowFor(runIds.succeeded)], false)
    expect(screen.queryByRole('columnheader', { name: 'Job' })).not.toBeInTheDocument()
    expect(screen.getByRole('columnheader', { name: 'Services' })).toBeInTheDocument()
  })

  it('shows the job id when the job has no title', () => {
    renderTable([{ ...rowFor(runIds.succeeded), job_name: '' }])
    expect(within(rowOf(109)).getByRole('link', { name: 'deploy-backend' })).toBeInTheDocument()
  })
})
