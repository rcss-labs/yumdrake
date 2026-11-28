import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { http, HttpResponse } from 'msw'
import { describe, expect, it } from 'vitest'
import type { Inventory } from '../../api/types'
import { renderApp } from '../../test/render'
import {
  connectionsManagerMe,
  delayNext,
  failNext,
  inventoryFixture,
  requests,
  server,
  servicesFixture,
  viewerMe,
} from '../../test/server'
import { InventoryPage } from './InventoryPage'

const noSourceInventory: Inventory = {
  commit: '',
  activated_at: null,
  checked_at: null,
  sync_error: '',
  sync_error_at: null,
  rejected: null,
  environments: [],
  services: [],
}

function showInventory(route = '/inventory', me = viewerMe) {
  return renderApp(<InventoryPage />, { route, me })
}

async function environmentsTable() {
  return screen.findByRole('table', { name: 'Environments' })
}

describe('the inventory header', () => {
  it('shows the active commit and when it was activated and last checked', async () => {
    showInventory()
    await environmentsTable()
    expect(screen.getByText('a1b2c3d')).toBeInTheDocument()
    expect(screen.getByText(/Activated/)).toBeInTheDocument()
    expect(screen.getByText(/Last checked/)).toBeInTheDocument()
  })
})

describe('the environments tab', () => {
  it('lists each environment with its label, AWS account and how many services exist there', async () => {
    showInventory()
    const table = await environmentsTable()
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows).toHaveLength(5)
    const first = within(rows[0]!).getAllByRole('cell')
    expect(first.map((cell) => cell.textContent)).toEqual(['hetzner-01', 'dev', '111111111111', '2'])
    const third = within(rows[2]!).getAllByRole('cell')
    expect(third.map((cell) => cell.textContent)).toEqual(['hetzner-03', 'dev', '111111111111', '1'])
    const last = within(rows[4]!).getAllByRole('cell')
    expect(last.map((cell) => cell.textContent)).toEqual(['prod-01', 'prod', '333333333333', '2'])
  })

  it('is shown in a tab panel and switches to services from the tab', async () => {
    const { location } = showInventory()
    await environmentsTable()
    expect(screen.getByRole('tab', { name: 'Environments', selected: true })).toBeInTheDocument()
    expect(screen.getByRole('tabpanel')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: 'Services' }))
    expect(location().search).toContain('tab=services')
    expect(await screen.findByRole('searchbox', { name: 'Find a service' })).toBeInTheDocument()
  })
})

describe('a rejected newer commit', () => {
  it('warns that the last good inventory is shown, lists the problems, and keeps the old data visible', async () => {
    showInventory()
    const warning = await screen.findByText(/last good inventory/i)
    expect(warning).toBeInTheDocument()
    expect(screen.getByText('ffee001')).toBeInTheDocument()
    expect(screen.getByText('services/billing.yaml:14')).toBeInTheDocument()
    expect(screen.getByText('ecs_service is required')).toBeInTheDocument()
    expect(screen.getByText('environments.yaml')).toBeInTheDocument()
    expect(await environmentsTable()).toBeInTheDocument()
  })

  it('shows no warning when nothing was rejected', async () => {
    server.use(http.get('/api/v1/inventory', () => HttpResponse.json({ ...inventoryFixture, rejected: null })))
    showInventory()
    await environmentsTable()
    expect(screen.queryByText(/last good inventory/i)).not.toBeInTheDocument()
  })
})

describe('no inventory source', () => {
  function withoutSource() {
    server.use(http.get('/api/v1/inventory', () => HttpResponse.json(noSourceInventory)))
  }

  it('says so and links a connections manager to the Git sources screen', async () => {
    withoutSource()
    showInventory('/inventory', connectionsManagerMe)
    expect(await screen.findByRole('heading', { name: 'No inventory source is set' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Set the inventory source' })).toHaveAttribute('href', '/admin/sources')
  })

  it('shows a rejected first commit and its problems instead of claiming no source is set', async () => {
    server.use(
      http.get('/api/v1/inventory', () =>
        HttpResponse.json({ ...noSourceInventory, rejected: inventoryFixture.rejected }),
      ),
    )
    showInventory('/inventory', connectionsManagerMe)
    expect(await screen.findByText('ecs_service is required')).toBeInTheDocument()
    expect(screen.getByText('ffee001')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'No inventory source is set' })).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'No inventory is active yet' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Set the inventory source' })).toHaveAttribute('href', '/admin/sources')
  })

  it('shows the sync error instead of claiming no source is set', async () => {
    server.use(
      http.get('/api/v1/inventory', () =>
        HttpResponse.json({ ...noSourceInventory, sync_error: 'GitHub could not be reached', sync_error_at: '2026-11-27T09:00:00Z' }),
      ),
    )
    showInventory()
    expect(await screen.findByText('GitHub could not be reached')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'No inventory source is set' })).not.toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'No inventory is active yet' })).toBeInTheDocument()
  })

  it('tells anyone else to ask an administrator, with no link', async () => {
    withoutSource()
    showInventory()
    expect(await screen.findByRole('heading', { name: 'No inventory source is set' })).toBeInTheDocument()
    expect(screen.getByText(/Ask someone who manages connections/)).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Set the inventory source' })).not.toBeInTheDocument()
  })
})

describe('Sync from git', () => {
  it('is hidden from people who cannot manage connections, and nothing is posted', async () => {
    showInventory()
    await environmentsTable()
    expect(screen.queryByRole('button', { name: 'Sync from git' })).not.toBeInTheDocument()
    expect(requests.some((request) => request.method === 'POST')).toBe(false)
  })

  it('posts the sync and refreshes the last checked time', async () => {
    showInventory('/inventory', connectionsManagerMe)
    await environmentsTable()
    await userEvent.click(screen.getByRole('button', { name: 'Sync from git' }))
    await waitFor(() => expect(requests.some((r) => r.method === 'POST' && r.path === '/api/v1/inventory/sync')).toBe(true))
    await waitFor(() => expect(requests.filter((r) => r.method === 'GET' && r.path === '/api/v1/inventory')).toHaveLength(2))
  })

  it('shows a loading button while the sync runs', async () => {
    showInventory('/inventory', connectionsManagerMe)
    await environmentsTable()
    delayNext('POST', '/api/v1/inventory/sync', 150)
    await userEvent.click(screen.getByRole('button', { name: 'Sync from git' }))
    expect(screen.getByRole('button', { name: 'Sync from git' })).toBeDisabled()
    await waitFor(() => expect(screen.getByRole('button', { name: 'Sync from git' })).toBeEnabled())
  })

  it('shows the error next to the button when the sync fails', async () => {
    showInventory('/inventory', connectionsManagerMe)
    await environmentsTable()
    failNext('POST', '/api/v1/inventory/sync', 502, 'bad_gateway', 'GitHub could not be reached')
    await userEvent.click(screen.getByRole('button', { name: 'Sync from git' }))
    expect(await screen.findByText('GitHub could not be reached')).toBeInTheDocument()
  })
})

describe('the services tab', () => {
  it('lists services with how many environments they are in, and narrows by search', async () => {
    showInventory('/inventory?tab=services')
    const list = await screen.findByRole('list', { name: 'Services' })
    expect(within(list).getByRole('button', { name: /backend-api/ })).toHaveTextContent('all environments')
    expect(within(list).getByRole('button', { name: /billing/ })).toHaveTextContent('4 of 5 environments')
    await userEvent.type(screen.getByRole('searchbox', { name: 'Find a service' }), 'bill')
    expect(within(list).queryByRole('button', { name: /backend-api/ })).not.toBeInTheDocument()
    expect(within(list).getByRole('button', { name: /billing/ })).toBeInTheDocument()
  })

  it('says no service matches, and what to do', async () => {
    showInventory('/inventory?tab=services')
    await userEvent.type(await screen.findByRole('searchbox', { name: 'Find a service' }), 'zzz')
    expect(screen.getByText('No service matches')).toBeInTheDocument()
  })

  it('asks for a service until one is chosen', async () => {
    showInventory('/inventory?tab=services')
    expect(await screen.findByText('Choose a service to see its environments.')).toBeInTheDocument()
  })

  it('shows the version rule and one block per environment when a service is chosen', async () => {
    const { location } = showInventory('/inventory?tab=services')
    await userEvent.click(await screen.findByRole('button', { name: /backend-api/ }))
    expect(location().search).toContain('service=backend-api')
    expect(await screen.findByRole('heading', { name: 'backend-api' })).toBeInTheDocument()
    expect(screen.getByText('^\\d+\\.\\d+\\.\\d+$')).toBeInTheDocument()
    const blocks = screen.getAllByRole('region')
    expect(blocks).toHaveLength(6) // five environments and the service's file
    const prod = screen.getByRole('region', { name: 'prod-01' })
    expect(within(prod).getByText('backend-api-prod-01')).toBeInTheDocument()
    expect(within(prod).getByText('333333333333.dkr.ecr.eu-central-1.amazonaws.com/backend-api')).toBeInTheDocument()
    expect(within(prod).getByText('backend-api.env')).toBeInTheDocument()
    expect(within(prod).getByText('example-org/env-config-repo/prod-01/backend-api/backend-api.env')).toBeInTheDocument()
    expect(within(prod).getByText('s3://prod-01-config/env/backend-api/backend-api.env')).toBeInTheDocument()
  })

  it('links the chosen service name to its page', async () => {
    showInventory('/inventory?tab=services&service=backend-api')
    const heading = await screen.findByRole('heading', { name: 'backend-api' })
    expect(within(heading).getByRole('link', { name: 'backend-api' })).toHaveAttribute('href', '/services/backend-api')
  })

  it("shows the service's file under its name", async () => {
    showInventory('/inventory?tab=services&service=backend-api')
    const file = await screen.findByRole('region', { name: 'services/backend-api.yaml' })
    expect(file).toHaveTextContent('name: backend-api')
  })

  it('shows no file block when no text is stored for the service', async () => {
    server.use(
      http.get('/api/v1/inventory/services/backend-api', () =>
        HttpResponse.json({ ...servicesFixture.find((service) => service.name === 'backend-api'), source: '' }),
      ),
    )
    showInventory('/inventory?tab=services&service=backend-api')
    await screen.findByRole('heading', { name: 'backend-api' })
    expect(screen.queryByRole('region', { name: 'services/backend-api.yaml' })).not.toBeInTheDocument()
  })

  it('shows a loading state while a chosen service loads', async () => {
    delayNext('GET', '/api/v1/inventory/services/backend-api', 100)
    showInventory('/inventory?tab=services&service=backend-api')
    await screen.findByRole('list', { name: 'Services' })
    expect(await screen.findByText('Loading')).toBeInTheDocument()
    expect(await screen.findByRole('heading', { name: 'backend-api' })).toBeInTheDocument()
  })

  it('shows TOML files, and copes with a service that has no env files', async () => {
    showInventory('/inventory?tab=services&service=billing')
    const block = await screen.findByRole('region', { name: 'hetzner-01' })
    expect(within(block).getByText('billing.toml')).toBeInTheDocument()
    expect(within(block).getByText('s3://main-config-bucket/hetzner-01/billing.toml')).toBeInTheDocument()
    expect(within(block).getByText('No env files')).toBeInTheDocument()
  })

  it('narrows to one environment and keeps the choice in the address', async () => {
    const { location } = showInventory('/inventory?tab=services&service=billing')
    await screen.findByRole('region', { name: 'hetzner-01' })
    const narrow = screen.getByRole('group', { name: 'Environment to show' })
    expect(within(narrow).getAllByRole('button').map((b) => b.textContent)).toEqual([
      'All',
      'hetzner-01',
      'hetzner-02',
      'preprod-01',
      'prod-01',
    ])
    await userEvent.click(within(narrow).getByRole('button', { name: 'prod-01' }))
    expect(location().search).toContain('env=prod-01')
    expect(screen.getAllByRole('region')).toHaveLength(2) // the environment and the service's file
    expect(screen.getByRole('region', { name: 'prod-01' })).toBeInTheDocument()
    await userEvent.click(within(narrow).getByRole('button', { name: 'All' }))
    expect(location().search).not.toContain('env=')
    expect(screen.getAllByRole('region')).toHaveLength(5) // four environments and the service's file
  })

  it('shows an error with a retry when the service cannot be loaded', async () => {
    failNext('GET', '/api/v1/inventory/services/billing', 500, 'internal', 'Could not read billing')
    showInventory('/inventory?tab=services&service=billing')
    expect(await screen.findByText('Could not read billing')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    expect(await screen.findByRole('region', { name: 'hetzner-01' })).toBeInTheDocument()
  })
})

describe('loading and failure', () => {
  it('shows a loading state, then the inventory', async () => {
    delayNext('GET', '/api/v1/inventory', 100)
    showInventory()
    expect(await screen.findByText('Loading')).toBeInTheDocument()
    await environmentsTable()
  })

  it('shows a readable error with a retry when the inventory cannot be loaded', async () => {
    failNext('GET', '/api/v1/inventory', 500, 'internal', 'The inventory could not be read')
    showInventory()
    expect(await screen.findByText('The inventory could not be read')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('button', { name: 'Try again' }))
    await environmentsTable()
  })
})
