import { act, screen, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { Route, Routes } from 'react-router-dom'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { QueueView } from '../../api/types'
import { queueFixture } from '../../test/handlers'
import { renderApp } from '../../test/render'
import { failNext, server } from '../../test/server'
import { QueuePage } from './QueuePage'

const path = '/api/v1/queue'

function show() {
  return renderApp(
    <Routes>
      <Route path="/queue" element={<QueuePage />} />
      <Route path="/runs/:id" element={<p>The run page</p>} />
    </Routes>,
    { route: '/queue' },
  )
}

function answer(change: (queue: QueueView) => void) {
  const queue = queueFixture()
  change(queue)
  server.use(http.get(path, () => HttpResponse.json(queue)))
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'], shouldAdvanceTime: true, now: new Date('2026-11-28T12:00:00Z') })
})
afterEach(() => {
  vi.useRealTimers()
})

describe('the queue page', () => {
  it('says how many runners there are and how many slots are busy', async () => {
    show()
    expect(await screen.findByText('2 runners, 5 of 16 slots busy')).toBeInTheDocument()
    expect(screen.getByText(/2 legs queued, oldest waiting 2m 0s/)).toBeInTheDocument()
  })

  it('uses the singular for one runner and says when nothing is queued', async () => {
    answer((queue) => {
      queue.runners = queue.runners.slice(0, 1)
      queue.queued_legs = 0
      queue.oldest_queued_at = null
    })
    show()
    expect(await screen.findByText('1 runner, 3 of 8 slots busy')).toBeInTheDocument()
    expect(screen.getByText('Nothing queued')).toBeInTheDocument()
  })

  it('warns when no runner has reported, naming how long that takes to count', async () => {
    answer((queue) => {
      queue.runners = []
      queue.free_slots = 0
    })
    show()
    const warning = await screen.findByRole('alert')
    expect(warning).toHaveTextContent('No runner has reported for 2 minutes. Queued runs will not start until one does.')
    expect(screen.queryByText(/slots busy/)).not.toBeInTheDocument()
  })

  it('lists active and waiting runs with the reason and how long they have waited', async () => {
    show()
    const table = await screen.findByRole('table', { name: 'Active and waiting runs' })
    const waiting = within(table).getByRole('link', { name: '#42' }).closest('tr') as HTMLElement
    expect(within(waiting).getByText('Deploy backend')).toBeInTheDocument()
    expect(within(waiting).getByText('hetzner-01')).toBeInTheDocument()
    expect(within(waiting).getByText('Queued')).toBeInTheDocument()
    expect(within(waiting).getByText('2m 14s')).toBeInTheDocument()
  })

  it('links a waiting reason to the run that blocks it', async () => {
    show()
    const table = await screen.findByRole('table', { name: 'Active and waiting runs' })
    const waiting = within(table).getByRole('link', { name: '#42' }).closest('tr') as HTMLElement
    expect(within(waiting).getByText(/Waiting for run/)).toBeInTheDocument()
    expect(within(waiting).getByRole('link', { name: '#41' })).toHaveAttribute('href', '/runs/41')
  })

  it('shows a reason that names no run as plain words', async () => {
    answer((queue) => {
      queue.runs[1]!.blocked_by_run_id = null
      queue.runs[1]!.wait_reason = 'waiting for a free runner'
    })
    show()
    expect(await screen.findByText('Waiting for a free runner')).toBeInTheDocument()
  })

  it('lists the locks with a shortened resource, the full one as a tooltip, and the holder', async () => {
    show()
    const table = await screen.findByRole('table', { name: 'Locks' })
    const full = 'ecs:111111111111:eu-west-1:cluster/main:service/billing-api'
    const resource = within(table).getByText(/^ecs:111111111111/)
    expect(resource).toHaveClass('mono')
    expect(resource).toHaveAttribute('title', full)
    expect(resource.textContent).toHaveLength(48)
    expect(resource.textContent).toContain('…')
    expect(within(table).getByRole('link', { name: '#41' })).toHaveAttribute('href', '/runs/41')
  })

  it('says so when there are no runs and no locks', async () => {
    answer((queue) => {
      queue.runs = []
      queue.locks = []
    })
    show()
    expect(await screen.findByText('No active or waiting runs')).toBeInTheDocument()
    expect(screen.getByText('No locks are held')).toBeInTheDocument()
  })

  it('shows an error with a retry when the first load fails', async () => {
    failNext('GET', path, 500, 'internal', 'Broken')
    show()
    expect(await screen.findByRole('button', { name: 'Try again' })).toBeInTheDocument()
  })

  it('refreshes every 5 seconds and keeps the last data with a note when a refresh fails', async () => {
    show()
    await screen.findByText('2 runners, 5 of 16 slots busy')
    failNext('GET', path, 500, 'internal', 'Broken')
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000)
    })
    expect(await screen.findByText(/Could not refresh\. Showing data from/)).toBeInTheDocument()
    expect(screen.getByText('2 runners, 5 of 16 slots busy')).toBeInTheDocument()
    // The next refresh works again and the note goes away.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(5_000)
    })
    await vi.waitFor(() => expect(screen.queryByText(/Could not refresh/)).not.toBeInTheDocument())
  })
})
