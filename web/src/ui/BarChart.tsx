import { useState } from 'react'
import './BarChart.css'
import './Loading.css' // the shared .ui-visually-hidden class

export interface BarChartSeries {
  key: string
  label: string
  color: string
}

export interface BarChartDay {
  /** Short, for the axis ("28 Nov"). */
  label: string
  /** Full, for the readout and the table ("28 November 2026"). */
  title: string
  values: { key: string; value: number }[]
}

export interface BarChartProps {
  days: BarChartDay[]
  /** In stacking order, bottom first. */
  series: BarChartSeries[]
  /** Height of the plot in pixels. */
  height: number
  /** Names the chart and its table. */
  caption: string
}

const maxTicks = 5

const valueOf = (day: BarChartDay, key: string) => day.values.find((entry) => entry.key === key)?.value ?? 0

/** Whole numbers from 0 to `max`, at most five of them. */
function ticksFor(max: number): number[] {
  if (max <= 0) return [0]
  const gaps = Math.min(max, maxTicks - 1)
  return Array.from(new Set(Array.from({ length: gaps + 1 }, (_, index) => Math.round((max * index) / gaps))))
}

export function BarChart({ days, series, height, caption }: BarChartProps) {
  const [shown, setShown] = useState<number | null>(null)

  const totals = days.map((day) => series.reduce((sum, entry) => sum + valueOf(day, entry.key), 0))
  const max = Math.max(0, ...totals)
  const readoutFor = (day: BarChartDay) =>
    `${day.title}: ${series.map((entry) => `${valueOf(day, entry.key)} ${entry.label.toLowerCase()}`).join(', ')}`
  const periodReadout = `${days.length} days: ${series
    .map((entry) => `${days.reduce((sum, day) => sum + valueOf(day, entry.key), 0)} ${entry.label.toLowerCase()}`)
    .join(', ')}`
  const focused = shown === null ? undefined : days[shown]
  const step = Math.ceil(days.length / 14)

  return (
    <div role="group" aria-label={caption} className="ui-chart">
      <p className="ui-chart-readout num" data-readout>
        {focused ? readoutFor(focused) : periodReadout}
      </p>
      <ul className="ui-chart-legend" aria-label="Legend">
        {series.map((entry) => (
          <li key={entry.key}>
            <span className="ui-chart-swatch" style={{ background: entry.color }} aria-hidden="true" />
            {entry.label}
          </li>
        ))}
      </ul>
      <div className="ui-chart-plot" style={{ height }}>
        {ticksFor(max).map((tick) => (
          <div key={tick} className="ui-chart-grid" style={{ bottom: max === 0 ? 0 : (tick / max) * height }} aria-hidden="true">
            <span data-tick>{tick}</span>
          </div>
        ))}
        {max === 0 ? (
          <p className="ui-chart-empty">No runs in this period</p>
        ) : (
          <div className="ui-chart-bars">
            {days.map((day, index) => (
              <button
                key={day.title}
                type="button"
                className="ui-chart-day"
                aria-label={readoutFor(day)}
                onMouseEnter={() => setShown(index)}
                onFocus={() => setShown(index)}
                onMouseLeave={() => setShown(null)}
                onBlur={() => setShown(null)}
              >
                {series
                  .slice()
                  .reverse()
                  .map((entry) => {
                    const value = valueOf(day, entry.key)
                    return value > 0 ? (
                      <span key={entry.key} data-series={entry.key} style={{ height: (value / max) * height, background: entry.color }} />
                    ) : null
                  })}
              </button>
            ))}
          </div>
        )}
      </div>
      {max > 0 && (
        <div className="ui-chart-axis" aria-hidden="true">
          {days.map((day, index) => (
            <span key={day.title}>{(days.length - 1 - index) % step === 0 ? day.label : ''}</span>
          ))}
        </div>
      )}
      <div className="ui-visually-hidden">
        <table aria-label={caption}>
          <thead>
            <tr>
              <th scope="col">Day</th>
              {series.map((entry) => (
                <th key={entry.key} scope="col">
                  {entry.label}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {days.map((day) => (
              <tr key={day.title}>
                <th scope="row">{day.title}</th>
                {series.map((entry) => (
                  <td key={entry.key}>{valueOf(day, entry.key)}</td>
                ))}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  )
}
