import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Route, Routes } from 'react-router-dom'
import type { Me, ServicePage as ServicePageBody } from '../../api/types'
import { runningCell, servicePageFixture } from '../../test/handlers'
import { renderApp } from '../../test/render'
import { mockState, server, viewerMe } from '../../test/server'
import { ServicePage } from './ServicePage'

const path = '/api/v1/services/backend-api'

function show(me?: Me) {
  return renderApp(
    <Routes>
      <Route path="/services/:name" element={<ServicePage />} />
      <Route path="/runs/:id" element={<p>The run page</p>} />
    </Routes>,
    { route: '/services/backend-api', me },
  )
}

function answer(change: (page: ServicePageBody) => void) {
  const service = mockState().services.find((candidate) => candidate.name === 'backend-api')!
  const page = servicePageFixture(service)
  change(page)
  server.use(http.get(path, () => HttpResponse.json(page)))
}

const restorer = (scope: Partial<Me['grants'][number]> = {}): Me => ({
  ...viewerMe,
  id: 77,
  name: 'Restorer',
  grants: [{ action: 'restore', label: '', environment: '', folder_id: 0, ...scope }],
})

const card = (environment: string) => screen.getByRole('region', { name: `Running in ${environment}` })

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['Date'], now: new Date('2026-11-28T12:00:00Z') })
})
afterEach(() => {
  vi.useRealTimers()
})

describe('the service page', () => {
  it('names the service and shows the facts of its inventory entry', async () => {
    show()
    expect(await screen.findByRole('heading', { level: 1, name: 'backend-api' })).toBeInTheDocument()
    expect(screen.getByText('Version pattern')).toBeInTheDocument()
    expect(screen.getByText('Sorted by')).toBeInTheDocument()
  })

  it('shows one card per environment with its version, markers, task definition and read time', async () => {
    show()
    const dev = within(await screen.findByRole('region', { name: 'Running in hetzner-01' }))
    expect(dev.getByText('dev')).toBeInTheDocument()
    expect(dev.getByText('1.4.2')).toHaveClass('mono')
    expect(dev.getByText('billing-api:142')).toBeInTheDocument()
    expect(dev.getByText('read 2 minutes ago')).toBeInTheDocument()
    expect(dev.queryByText('Not deployed by Yumdrake')).not.toBeInTheDocument()
    const prod = within(card('prod-01'))
    expect(prod.getByText('1.4.1')).toBeInTheDocument()
    expect(prod.getByText('Not deployed by Yumdrake')).toBeInTheDocument()
  })

  it('says when an environment could not be read or is not set up, without hiding the others', async () => {
    answer((page) => {
      page.environments[0]!.cell = { ...runningCell(''), state: 'error', error: 'AccessDenied' }
      page.environments[1]!.cell = { ...runningCell(''), state: 'absent' }
    })
    show()
    await screen.findByRole('region', { name: 'Running in hetzner-01' })
    expect(within(card('hetzner-01')).getByText('Could not read')).toBeInTheDocument()
    expect(within(card('hetzner-01')).getByText('AccessDenied')).toHaveClass('mono')
    expect(within(card('prod-01')).getByText('Not set up')).toBeInTheDocument()
  })

  it('shows the inventory block for the service', async () => {
    show()
    await screen.findByRole('region', { name: 'Running in hetzner-01' })
    expect(await screen.findAllByText('ECS service')).not.toHaveLength(0)
  })

  it('answers with the not found page for a service that does not exist', async () => {
    renderApp(
      <Routes>
        <Route path="/services/:name" element={<ServicePage />} />
      </Routes>,
      { route: '/services/nope' },
    )
    expect(await screen.findByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
  })
})

describe('deployments and config history', () => {
  it('lists deployments with links to their runs', async () => {
    show()
    const table = await screen.findByRole('table', { name: 'Deployments' })
    const row = within(table).getAllByRole('row')[1]!
    expect(within(row).getByRole('link', { name: '#12' })).toHaveAttribute('href', '/runs/12')
    expect(within(row).getByText('hetzner-01')).toBeInTheDocument()
    expect(within(row).getByText('1.4.2')).toHaveClass('mono')
    expect(within(row).getByText('Succeeded')).toBeInTheDocument()
    expect(within(row).getByText('Ada')).toBeInTheDocument()
  })

  it('says so when there are no deployments', async () => {
    answer((page) => {
      page.deployments = []
    })
    show()
    expect(await screen.findByText('No deployments yet')).toBeInTheDocument()
  })

  it('lists one config row per item with the destination as its title and the checksum in mono', async () => {
    answer((page) => {
      page.config[0]!.items.push({ kind: 'toml', name: 'app.toml', destination: '/etc/app/app.toml', checksum: '0123456789ab' })
    })
    show()
    const table = await screen.findByRole('table', { name: 'Config history' })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(2)
    expect(within(rows[0]!).getByRole('link', { name: '#12' })).toHaveAttribute('href', '/runs/12')
    expect(within(rows[0]!).getByText('app.env')).toHaveAttribute('title', '/etc/app/app.env')
    expect(within(rows[0]!).getByText('a1b2c3d4e5f6')).toHaveClass('mono')
    expect(within(rows[1]!).getByText('app.toml')).toHaveAttribute('title', '/etc/app/app.toml')
  })

  it('says so when no config changed', async () => {
    answer((page) => {
      page.config = []
    })
    show()
    expect(await screen.findByText('No config changes recorded')).toBeInTheDocument()
  })
})

describe('Restore', () => {
  it('is shown for the person who may restore, and only where a restore source exists', async () => {
    show(restorer())
    await screen.findByRole('region', { name: 'Running in hetzner-01' })
    expect(within(card('hetzner-01')).getByRole('button', { name: 'Restore' })).toBeInTheDocument()
    // prod-01 has no restore_from_run_id in the fixture.
    expect(within(card('prod-01')).queryByRole('button', { name: 'Restore' })).not.toBeInTheDocument()
  })

  it('is not shown without the restore permission, even where a source exists', async () => {
    show(viewerMe)
    await screen.findByRole('region', { name: 'Running in hetzner-01' })
    expect(screen.queryByRole('button', { name: 'Restore' })).not.toBeInTheDocument()
  })

  it("shows Restore to a person whose restore grant is scoped to the source run's folder", async () => {
    show(restorer({ folder_id: 1 }))
    await screen.findByRole('region', { name: 'Running in hetzner-01' })
    expect(within(card('hetzner-01')).getByRole('button', { name: 'Restore' })).toBeInTheDocument()
  })

  it('follows the scope of the grant for each environment', async () => {
    answer((page) => {
      page.environments[1]!.restore_from_run_id = 9
      page.environments[1]!.restore_from_folder_id = 1
    })
    show(restorer({ label: 'prod' }))
    await screen.findByRole('region', { name: 'Running in hetzner-01' })
    expect(within(card('hetzner-01')).queryByRole('button', { name: 'Restore' })).not.toBeInTheDocument()
    expect(within(card('prod-01')).getByRole('button', { name: 'Restore' })).toBeInTheDocument()
  })

  it('asks first, then plans a restore of this service from the source run and opens the new run', async () => {
    const user = userEvent.setup()
    const posted: unknown[] = []
    server.use(
      http.post('/api/v1/runs/12/restore', async ({ request }) => {
        posted.push(await request.json())
        return HttpResponse.json({ id: 200 }, { status: 201 })
      }),
    )
    const { location } = show(restorer())
    await user.click(await within(await screen.findByRole('region', { name: 'Running in hetzner-01' })).findByRole('button', { name: 'Restore' }))
    const dialog = screen.getByRole('dialog', { name: 'Restore 1 service?' })
    expect(dialog).toHaveTextContent('This plans a restore of backend-api to what they ran before. Nothing changes until you start the restore run.')
    expect(posted).toEqual([])
    await user.click(within(dialog).getByRole('button', { name: 'Plan the restore' }))
    expect(await screen.findByText('The run page')).toBeInTheDocument()
    expect(posted).toEqual([{ services: ['backend-api'] }])
    expect(location().pathname).toBe('/runs/200')
  })

  it('shows what went wrong and stays on the page when the restore is refused', async () => {
    const user = userEvent.setup()
    server.use(
      http.post('/api/v1/runs/12/restore', () =>
        HttpResponse.json({ error: { code: 'conflict', message: 'nothing in it can be restored' } }, { status: 409 }),
      ),
    )
    show(restorer())
    await user.click(await within(await screen.findByRole('region', { name: 'Running in hetzner-01' })).findByRole('button', { name: 'Restore' }))
    await user.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Plan the restore' }))
    expect(await screen.findByText('nothing in it can be restored')).toBeInTheDocument()
    expect(screen.queryByText('The run page')).not.toBeInTheDocument()
  })
})
