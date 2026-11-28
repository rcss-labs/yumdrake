import { render, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { BarChart } from './BarChart'

const series = [
  { key: 'succeeded', label: 'Succeeded', color: '#5B8DC9' },
  { key: 'failed', label: 'Failed', color: '#D03A26' },
  { key: 'other', label: 'Other', color: '#aaa' },
]

const days = [
  { label: '26 Nov', title: '26 November 2026', values: [{ key: 'succeeded', value: 3 }, { key: 'failed', value: 1 }, { key: 'other', value: 0 }] },
  { label: '27 Nov', title: '27 November 2026', values: [{ key: 'succeeded', value: 0 }, { key: 'failed', value: 0 }, { key: 'other', value: 0 }] },
  { label: '28 Nov', title: '28 November 2026', values: [{ key: 'succeeded', value: 17 }, { key: 'failed', value: 3 }, { key: 'other', value: 1 }] },
]

function show(data = days) {
  return render(<BarChart caption="Runs per day" days={data} series={series} height={200} />)
}

describe('BarChart', () => {
  it('reads out the totals for the period until a day is focused', () => {
    show()
    expect(screen.getByText('3 days: 20 succeeded, 4 failed, 1 other')).toBeInTheDocument()
  })

  it('reads out a day when its button is focused and goes back when focus leaves', async () => {
    show()
    const button = screen.getByRole('button', { name: '26 November 2026: 3 succeeded, 1 failed, 0 other' })
    button.focus()
    expect(await screen.findByText('26 November 2026: 3 succeeded, 1 failed, 0 other', { selector: '[data-readout]' })).toBeInTheDocument()
    button.blur()
    expect(await screen.findByText('3 days: 20 succeeded, 4 failed, 1 other')).toBeInTheDocument()
  })

  it('reads out a day on hover', async () => {
    show()
    await userEvent.hover(screen.getByRole('button', { name: /^28 November 2026/ }))
    expect(screen.getByText('28 November 2026: 17 succeeded, 3 failed, 1 other', { selector: '[data-readout]' })).toBeInTheDocument()
  })

  it('draws no bar for a day with no runs', () => {
    show()
    const empty = screen.getByRole('button', { name: /^27 November 2026/ })
    expect(empty.querySelectorAll('[data-series]')).toHaveLength(0)
    const busy = screen.getByRole('button', { name: /^26 November 2026/ })
    expect(busy.querySelectorAll('[data-series]')).toHaveLength(2)
  })

  it('has a legend entry per series', () => {
    show()
    const legend = screen.getByRole('list', { name: 'Legend' })
    expect(within(legend).getAllByRole('listitem').map((item) => item.textContent)).toEqual(['Succeeded', 'Failed', 'Other'])
  })

  it('has whole-number ticks from zero to the largest day, at most five', () => {
    const { container } = show()
    const ticks = Array.from(container.querySelectorAll('[data-tick]')).map((tick) => tick.textContent)
    expect(ticks[0]).toBe('0')
    expect(ticks[ticks.length - 1]).toBe('21')
    expect(ticks.length).toBeLessThanOrEqual(5)
    ticks.forEach((tick) => expect(tick).toMatch(/^\d+$/))
  })

  it('has a hidden table with the same numbers as the chart', () => {
    show()
    const table = screen.getByRole('table', { name: 'Runs per day' })
    const rows = within(table).getAllByRole('row')
    expect(within(rows[0]!).getAllByRole('columnheader').map((cell) => cell.textContent)).toEqual(['Day', 'Succeeded', 'Failed', 'Other'])
    const cells = rows.slice(1).map((row) => [
      within(row).getByRole('rowheader').textContent,
      ...within(row).getAllByRole('cell').map((cell) => cell.textContent),
    ])
    expect(cells).toEqual([
      ['26 November 2026', '3', '1', '0'],
      ['27 November 2026', '0', '0', '0'],
      ['28 November 2026', '17', '3', '1'],
    ])
    expect(table.closest('.ui-visually-hidden')).not.toBeNull()
  })

  it('shows an empty state instead of bars when every day is zero', () => {
    const { container } = show(days.map((day) => ({ ...day, values: day.values.map((v) => ({ ...v, value: 0 })) })))
    expect(screen.getByText('No runs in this period')).toBeInTheDocument()
    expect(container.querySelectorAll('[data-series]')).toHaveLength(0)
    expect(Array.from(container.querySelectorAll('[data-tick]')).map((tick) => tick.textContent)).toEqual(['0'])
  })
})
