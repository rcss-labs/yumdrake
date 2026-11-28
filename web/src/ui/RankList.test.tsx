import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { RankList } from './RankList'

const items = [
  { key: 'billing-api', label: 'billing-api', value: 4, detail: 'Health check failed', href: '/services/billing-api' },
  { key: 'search', label: 'search', value: 1 },
]

function show(list = items) {
  return render(
    <MemoryRouter>
      <RankList items={list} valueLabel={(n) => `${n} ${n === 1 ? 'failure' : 'failures'}`} empty="No failed deployments in this period" />
    </MemoryRouter>,
  )
}

describe('RankList', () => {
  it('shows each label as a link, its value text and its detail', () => {
    show()
    expect(screen.getByRole('link', { name: 'billing-api' })).toHaveAttribute('href', '/services/billing-api')
    expect(screen.getByText('4 failures')).toBeInTheDocument()
    expect(screen.getByText('1 failure')).toBeInTheDocument()
    expect(screen.getByText('Health check failed')).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'search' })).not.toBeInTheDocument()
  })

  it('sizes each bar against the largest value', () => {
    const { container } = show()
    const bars = Array.from(container.querySelectorAll<HTMLElement>('.ui-rank-bar > span')).map((bar) => bar.style.width)
    expect(bars).toEqual(['100%', '25%'])
  })

  it('shows the empty text when there are no items', () => {
    show([])
    expect(screen.getByText('No failed deployments in this period')).toBeInTheDocument()
  })
})
