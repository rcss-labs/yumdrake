import { act, screen, within } from '@testing-library/react'
import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { App } from '../../App'
import type { SystemHealth } from '../../api/types'
import { healthFixture } from '../../test/handlers'
import { renderApp } from '../../test/render'
import { connectionsManagerMe, failNext, jobManagerMe, server } from '../../test/server'

const path = '/api/v1/system/health'

function show(me = connectionsManagerMe) {
  return renderApp(<App />, { route: '/admin/health', me })
}

function answer(change: (health: SystemHealth) => void) {
  const health = healthFixture()
  change(health)
  server.use(http.get(path, () => HttpResponse.json(health)))
}

const section = (name: string) => screen.findByRole('region', { name })

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date', 'setInterval', 'clearInterval'], shouldAdvanceTime: true, now: new Date('2026-11-28T12:00:00Z') })
})
afterEach(() => {
  vi.useRealTimers()
})

describe('the system health page', () => {
  it('says everything is working when there are no problems', async () => {
    show()
    expect(await screen.findByText('Everything is working')).toBeInTheDocument()
  })

  it('shows each problem as an error note, in the order given', async () => {
    answer((health) => {
      health.problems = ['No runner has reported for 2 minutes.', '1 notification failed in the last day.']
    })
    show()
    await screen.findByText('No runner has reported for 2 minutes.')
    const alerts = screen.getAllByRole('alert').map((alert) => alert.textContent)
    expect(alerts).toEqual(['No runner has reported for 2 minutes.', '1 notification failed in the last day.'])
    expect(screen.queryByText('Everything is working')).not.toBeInTheDocument()
  })

  it('lists the runners with their load, and says when there are none', async () => {
    show()
    const runners = within(await section('Runners'))
    const row = runners.getByText('runner-a').closest('tr') as HTMLElement
    expect(within(row).getByText('3 of 8')).toBeInTheDocument()
    expect(within(row).getByText('0.9.0')).toBeInTheDocument()
  })

  it('says that no runner has reported when the list is empty', async () => {
    answer((health) => {
      health.runners = []
    })
    show()
    const runners = within(await section('Runners'))
    expect(runners.getByText('No runner has reported for 2 minutes.')).toBeInTheDocument()
  })

  it('summarises the queue', async () => {
    show()
    const queue = within(await section('Queue'))
    expect(queue.getByText('Queued legs')).toBeInTheDocument()
    expect(queue.getByText('Active runs')).toBeInTheDocument()
    expect(queue.getByText('Oldest queued')).toBeInTheDocument()
  })

  it('shows the inventory and each sync job with its state', async () => {
    answer((health) => {
      health.sync.jobs.push({ id: 2, job_id: 'deploy-backend', title: 'Deploy backend', checked_at: null, error: 'repository not found', error_at: '2026-11-28T11:59:00Z' })
    })
    show()
    const sync = within(await section('Sync'))
    expect(sync.getByText('Inventory')).toBeInTheDocument()
    expect(sync.getByText('Deploy billing')).toBeInTheDocument()
    expect(sync.getAllByText('OK')).toHaveLength(2)
    expect(sync.getByText('repository not found')).toBeInTheDocument()
  })

  it('shows each connection and marks a failing one', async () => {
    answer((health) => {
      health.connections[1] = { kind: 'github', name: 'rcss-labs', last_ok_at: null, last_error_at: '2026-11-28T11:59:00Z', last_error_code: 'bad_credentials', failing: true }
    })
    show()
    const connections = within(await section('Connections'))
    expect(connections.getByText('homelab')).toBeInTheDocument()
    const failing = connections.getByText('rcss-labs').closest('tr') as HTMLElement
    expect(within(failing).getByText('Failed')).toBeInTheDocument()
    expect(within(failing).getByText('bad_credentials')).toBeInTheDocument()
  })

  it('shows the notification backlog', async () => {
    answer((health) => {
      health.notifications = { pending: 3, failed_last_day: 1, oldest_pending_at: '2026-11-28T11:00:00Z' }
    })
    show()
    const notifications = within(await section('Notifications'))
    expect(notifications.getByText('3')).toBeInTheDocument()
    expect(notifications.getByText('1 hour ago')).toBeInTheDocument()
  })

  it('shows the three failure counters for the last hour and day, in words', async () => {
    show()
    const failures = within(await section('Recent failures'))
    const row = failures.getByText('Sync failures').closest('tr') as HTMLElement
    expect(within(row).getAllByRole('cell').map((cell) => cell.textContent)).toEqual(['Sync failures', '0', '2'])
    expect(failures.getByText('Expired leases')).toBeInTheDocument()
    expect(failures.getByText('Log batches not stored')).toBeInTheDocument()
  })

  it('is not offered to a person without manage_connections', async () => {
    show(jobManagerMe)
    expect(screen.queryByRole('tab', { name: 'System health' })).not.toBeInTheDocument()
    await vi.waitFor(() => expect(screen.queryByText('Everything is working')).not.toBeInTheDocument())
  })

  it('keeps the last data with a note when a refresh fails', async () => {
    show()
    await screen.findByText('Everything is working')
    failNext('GET', path, 500, 'internal', 'Broken')
    await act(async () => {
      await vi.advanceTimersByTimeAsync(15_000)
    })
    expect(await screen.findByText(/Could not refresh\. Showing data from/)).toBeInTheDocument()
    expect(screen.getByText('Everything is working')).toBeInTheDocument()
  })
})
