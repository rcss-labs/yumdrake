import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { App } from '../../App'
import { renderApp } from '../../test/render'
import {
  accessManagerMe,
  adminMe,
  auditorMe,
  connectionsManagerMe,
  jobManagerMe,
  requests,
} from '../../test/server'
import type { Me } from '../../api/types'

async function tabNames(me: Me, route = '/admin/users') {
  const rendered = renderApp(<App />, { route, me })
  const list = await screen.findByRole('tablist', { name: 'Admin sections' })
  return { rendered, names: within(list).getAllByRole('tab').map((tab) => tab.textContent) }
}

describe('the admin page', () => {
  it('shows every section to an administrator, in order', async () => {
    const { names } = await tabNames(adminMe)
    expect(names).toEqual([
      'Users',
      'Groups and permissions',
      'Single sign-on',
      'Freezes',
      'AWS accounts',
      'GitHub',
      'Git sources',
      'Slack',
      'System health',
      'Audit log',
    ])
  })

  it('shows only the access sections to an access manager', async () => {
    const { names } = await tabNames(accessManagerMe)
    expect(names).toEqual(['Users', 'Groups and permissions', 'Single sign-on', 'Freezes'])
  })

  it('shows only the connection sections to a connections manager', async () => {
    const { names } = await tabNames(connectionsManagerMe, '/admin/aws')
    expect(names).toEqual(['AWS accounts', 'GitHub', 'Git sources', 'Slack', 'System health'])
  })

  it('shows only the audit log to an auditor', async () => {
    const { names } = await tabNames(auditorMe, '/admin/audit')
    expect(names).toEqual(['Audit log'])
  })

  it('sends /admin to the first section the person may see', async () => {
    const { location } = renderApp(<App />, { route: '/admin', me: connectionsManagerMe })
    await screen.findByRole('tablist', { name: 'Admin sections' })
    expect(location().pathname).toBe('/admin/aws')
    expect(screen.getByRole('tab', { name: 'AWS accounts', selected: true })).toBeInTheDocument()
  })

  it('shows the selected section in a tab panel and follows a tab click', async () => {
    const { rendered } = await tabNames(adminMe)
    expect(screen.getByRole('tabpanel')).toBeInTheDocument()
    await userEvent.click(screen.getByRole('tab', { name: 'GitHub' }))
    expect(rendered.location().pathname).toBe('/admin/github')
    expect(screen.getByRole('tab', { name: 'GitHub', selected: true })).toBeInTheDocument()
  })

  it('shows no access for a section the person may not see, and requests nothing from admin', async () => {
    renderApp(<App />, { route: '/admin/aws', me: accessManagerMe })
    expect(await screen.findByText('You do not have access')).toBeInTheDocument()
    expect(screen.queryByRole('tablist')).not.toBeInTheDocument()
    expect(requests.filter((request) => request.path.startsWith('/api/v1/admin/'))).toEqual([])
  })

  it('shows no access at /admin to a person who manages only jobs', async () => {
    const { location } = renderApp(<App />, { route: '/admin', me: jobManagerMe })
    expect(await screen.findByText('You do not have access')).toBeInTheDocument()
    expect(location().pathname).toBe('/admin')
    expect(requests.filter((request) => request.path.startsWith('/api/v1/admin/'))).toEqual([])
  })

  it('shows page not found for an unknown section', async () => {
    renderApp(<App />, { route: '/admin/nonsense', me: adminMe })
    expect(await screen.findByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
  })
})
