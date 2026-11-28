import { Link, useSearchParams } from 'react-router-dom'
import type { Overview, OverviewAttention, OverviewEnvironment } from '../../api/types'
import {
  BarChart,
  Card,
  DataTable,
  ErrorState,
  Loading,
  PageHeader,
  RankList,
  Segmented,
  Select,
  StatTile,
  StatusPill,
  Tag,
  Time,
  formatDuration,
  relativeTime,
} from '../../ui'
import type { DataColumn } from '../../ui'
import { useInventory } from '../inventory/queries'
import { useOverview } from './queries'
import './overview.css'

const periods = [7, 14, 30]
const defaultPeriod = 14
const noRuns = 'No runs in this period'

const series = [
  { key: 'succeeded', label: 'Succeeded', color: '#5B8DC9' },
  { key: 'failed', label: 'Failed', color: '#D03A26' },
  { key: 'other', label: 'Other', color: 'var(--mute)' },
]

const plural = (count: number, unit: string) => `${count} ${unit}${count === 1 ? '' : 's'}`

/** "28 Nov" and "28 November 2026" from "2026-11-28", read as that calendar day in any time zone. */
function dayLabels(date: string): { label: string; title: string } {
  const parsed = new Date(`${date}T00:00:00Z`)
  if (Number.isNaN(parsed.getTime())) return { label: date, title: date }
  const format = (options: Intl.DateTimeFormatOptions) => parsed.toLocaleDateString('en-GB', { ...options, timeZone: 'UTC' })
  return { label: format({ day: 'numeric', month: 'short' }), title: format({ day: 'numeric', month: 'long', year: 'numeric' }) }
}

function runsComparison(data: Overview, period: string): string {
  const change = data.runs - data.prev_runs
  if (change > 0) return `+${change} vs ${period}`
  if (change < 0) return `${-change} fewer than ${period}`
  return `Same as ${period}`
}

function rateComparison(data: Overview, period: string): string {
  if (data.success_rate === null) return ''
  if (data.prev_success_rate === null) return `No finished runs in ${period}`
  const change = Math.round(data.success_rate * 100) - Math.round(data.prev_success_rate * 100)
  if (change === 0) return `Same as ${period}`
  return `${plural(Math.abs(change), 'point')} ${change > 0 ? 'higher' : 'lower'} than ${period}`
}

function durationComparison(data: Overview, period: string): string {
  if (data.median_duration_seconds === null) return ''
  if (data.prev_median_duration_seconds === null) return `No finished runs in ${period}`
  const change = data.median_duration_seconds - data.prev_median_duration_seconds
  if (change === 0) return `Same as ${period}`
  return `${formatDuration(Math.abs(change) * 1000)} ${change < 0 ? 'faster' : 'slower'} than ${period}`
}

function Tiles({ data, period }: { data: Overview; period: string }) {
  const hasRuns = data.runs > 0
  return (
    <div className="ov-tiles">
      <StatTile
        label="Runs"
        value={hasRuns ? data.runs : undefined}
        empty={noRuns}
        comparison={hasRuns ? runsComparison(data, period) : undefined}
      />
      <StatTile
        label="Success rate"
        value={data.success_rate === null ? undefined : `${Math.round(data.success_rate * 100)}%`}
        empty={noRuns}
        comparison={rateComparison(data, period)}
      />
      <StatTile
        label="Median duration"
        value={data.median_duration_seconds === null ? undefined : formatDuration(data.median_duration_seconds * 1000)}
        empty={noRuns}
        comparison={durationComparison(data, period)}
      />
      <StatTile
        label="Waiting for approval"
        value={data.waiting_approval}
        comparison={
          data.waiting_approval > 0 && data.oldest_waiting ? `Oldest requested ${relativeTime(data.oldest_waiting)}` : 'Nothing is waiting'
        }
      />
    </div>
  )
}

function Attention({ items }: { items: OverviewAttention[] }) {
  if (items.length === 0) return <p className="ov-empty">Nothing needs attention</p>
  return (
    <ul className="ov-attention">
      {items.map((item) => (
        <li key={item.run_id}>
          <Link to={`/runs/${item.run_id}`} className="ov-att">
            <StatusPill status={item.state} />
            <span className="ov-att-main">
              <span className="ov-att-title">
                {item.job} in <span className="mono">{item.environment}</span>
              </span>
              <span className="ov-sub">{item.reason}</span>
            </span>
            <span className="ov-sub ov-nowrap">
              {item.by && `${item.by}, `}
              <Time iso={item.at} />
            </span>
          </Link>
        </li>
      ))}
    </ul>
  )
}

const environmentColumns: DataColumn<OverviewEnvironment>[] = [
  { key: 'environment', header: 'Environment', render: (row) => <span className="mono">{row.environment}</span> },
  { key: 'label', header: 'Label', render: (row) => <Tag>{row.label}</Tag> },
  {
    key: 'run',
    header: 'Last run',
    render: (row) =>
      row.run_id === null ? <span className="ov-sub">No runs yet</span> : <Link to={`/runs/${row.run_id}`}>#{row.run_id}</Link>,
  },
  { key: 'state', header: 'State', render: (row) => (row.run_id === null ? null : <StatusPill status={row.state} />) },
  { key: 'job', header: 'Job', render: (row) => row.job },
  { key: 'at', header: 'When', className: 'ov-nowrap', render: (row) => (row.at ? <Time iso={row.at} /> : null) },
]

export function OverviewPage() {
  const [params, setParams] = useSearchParams()
  const label = params.get('label') ?? ''
  const requested = Number(params.get('days'))
  const days = periods.includes(requested) ? requested : defaultPeriod
  const overview = useOverview(label, days)
  const inventory = useInventory()

  const labels = Array.from(new Set((inventory.data?.environments ?? []).map((environment) => environment.label)))
  const options = [{ value: '', label: 'All environments' }, ...labels.map((name) => ({ value: name, label: name }))]

  function change(name: 'label' | 'days', value: string) {
    const next = new URLSearchParams(params)
    if (value === '' || (name === 'days' && value === String(defaultPeriod))) next.delete(name)
    else next.set(name, value)
    setParams(next, { replace: true })
  }

  const filters = (
    <div className="ov-filters">
      <Segmented label="Environment label" options={options} value={label} onChange={(value) => change('label', value)} />
      <Select aria-label="Period" size="sm" value={String(days)} onChange={(event) => change('days', event.target.value)}>
        {periods.map((period) => (
          <option key={period} value={period}>
            {period} days
          </option>
        ))}
      </Select>
    </div>
  )
  const header = <PageHeader title="Overview" actions={filters} />

  if (overview.isPending) {
    return (
      <>
        {header}
        <Loading />
      </>
    )
  }
  if (overview.isError) {
    return (
      <>
        {header}
        <ErrorState error={overview.error} onRetry={() => void overview.refetch()} />
      </>
    )
  }

  const data = overview.data
  // While a new filter loads, the previous answer stays on screen: name the period of that answer, not the one asked for.
  const period = `the previous ${data.days.length} days`
  const stale = overview.isPlaceholderData
  const chartDays = data.days.map((day) => ({
    ...dayLabels(day.date),
    values: [
      { key: 'succeeded', value: day.succeeded },
      { key: 'failed', value: day.failed },
      { key: 'other', value: day.other },
    ],
  }))

  return (
    <>
      {header}
      <div className={stale ? 'ov-stale' : undefined} aria-busy={stale}>
      <Tiles data={data} period={period} />
      <div className="ov-row">
        <Card className="ov-wide">
          <h2>Runs per day</h2>
          <BarChart caption="Runs per day" days={chartDays} series={series} height={200} />
        </Card>
        <Card className="ov-narrow">
          <h2>Services that failed most</h2>
          <p className="ov-sub ov-lead">Failed deployments per service in the period</p>
          <RankList
            items={data.top_failing.map((item) => ({
              key: item.service,
              label: item.service,
              value: item.failures,
              detail: item.common_reason,
              href: `/services/${encodeURIComponent(item.service)}`,
            }))}
            valueLabel={(count) => plural(count, 'failure')}
            empty="No failed deployments in this period"
          />
        </Card>
      </div>
      <div className="ov-row">
        <Card className="ov-narrow">
          <h2>Needs attention</h2>
          <p className="ov-sub ov-lead">Latest runs that failed and were not fixed by a later run, and runs waiting for approval</p>
          <Attention items={data.attention} />
        </Card>
        <Card flush className="ov-wide">
          <h2 className="ov-flush-title">Environments</h2>
          <DataTable
            label="Environments"
            columns={environmentColumns}
            rows={data.environments}
            rowKey={(row) => row.environment}
            minWidth={560}
            empty={<p className="ov-empty">No environments to show</p>}
          />
        </Card>
      </div>
      </div>
    </>
  )
}
