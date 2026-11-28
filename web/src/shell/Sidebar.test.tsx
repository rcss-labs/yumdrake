import { act, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { App } from '../App'
import { renderApp } from '../test/render'
import { adminMe, jobManagerMe, mockState, requests, viewerMe } from '../test/server'

afterEach(() => vi.useRealTimers())

async function approvalsLink() {
  const nav = await screen.findByRole('navigation', { name: 'Main' })
  return nav.querySelector<HTMLAnchorElement>('a[href="/approvals"]') as HTMLAnchorElement
}

describe('the approvals count in the sidebar', () => {
  it('shows how many waiting runs the person may approve', async () => {
    renderApp(<App />, { me: adminMe })
    await waitFor(async () => expect((await approvalsLink()).textContent).toBe('Approvals1'))
    expect(await approvalsLink()).toHaveAccessibleName('Approvals, 1 waiting')
  })

  it('does not count runs the person may not approve', async () => {
    renderApp(<App />, { me: jobManagerMe })
    await approvalsLink()
    await waitFor(() => expect(requests.some((request) => request.path === '/api/v1/approvals')).toBe(true))
    await new Promise((resolve) => setTimeout(resolve, 30))
    expect((await approvalsLink()).textContent).toBe('Approvals')
  })

  it('shows no count when nothing is waiting', async () => {
    const run = mockState().runs.find((candidate) => candidate.id === 103)
    if (run) run.state = 'queued'
    renderApp(<App />, { me: viewerMe })
    await approvalsLink()
    await waitFor(() => expect(requests.some((request) => request.path === '/api/v1/approvals')).toBe(true))
    await new Promise((resolve) => setTimeout(resolve, 30))
    expect((await approvalsLink()).textContent).toBe('Approvals')
  })

  it('asks again every 30 seconds', async () => {
    // Timers are faked before the page renders, so the query's interval is the fake one.
    vi.useFakeTimers({ shouldAdvanceTime: true })
    renderApp(<App />, { me: adminMe })
    await waitFor(async () => expect((await approvalsLink()).textContent).toBe('Approvals1'))
    const run = mockState().runs.find((candidate) => candidate.id === 103)
    if (run) run.state = 'queued'
    await act(async () => {
      await vi.advanceTimersByTimeAsync(30_000)
    })
    await waitFor(async () => expect((await approvalsLink()).textContent).toBe('Approvals'))
  })
})

describe('the Queue item in the sidebar', () => {
  it('sits right under Runs', async () => {
    renderApp(<App />, { me: viewerMe })
    const nav = await screen.findByRole('navigation', { name: 'Main' })
    const links = Array.from(nav.querySelectorAll('a')).map((link) => link.getAttribute('href'))
    expect(links.indexOf('/queue')).toBe(links.indexOf('/runs') + 1)
  })
})
