import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { StatTile } from './StatTile'

describe('StatTile', () => {
  it('shows the label, the value and the comparison', () => {
    render(<StatTile label="Runs" value={24} comparison="+3 vs the previous 14 days" />)
    expect(screen.getByText('Runs')).toBeInTheDocument()
    expect(screen.getByText('24')).toBeInTheDocument()
    expect(screen.getByText('+3 vs the previous 14 days')).toBeInTheDocument()
  })

  it('shows the empty text instead of a value when there is none', () => {
    render(<StatTile label="Success rate" empty="No runs in this period" />)
    expect(screen.getByText('No runs in this period')).toBeInTheDocument()
  })

  it('still shows a value of zero', () => {
    render(<StatTile label="Waiting for approval" value={0} empty="No runs in this period" />)
    expect(screen.getByText('0')).toBeInTheDocument()
    expect(screen.queryByText('No runs in this period')).not.toBeInTheDocument()
  })
})
