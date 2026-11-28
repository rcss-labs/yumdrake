import { Link } from 'react-router-dom'
import type { RunSummary } from '../../api/types'
import { DataTable, legStatusLabels, ServiceStrip, StatusPill, Tag, Time, formatDuration } from '../../ui'
import type { DataColumn } from '../../ui'
import './runs.css'

const shownFailures = 2
const shownVersions = 3

function Services({ run }: { run: RunSummary }) {
  const services = run.services
  if (services.length === 0) return <span className="runs-quiet">None</span>
  const succeeded = services.filter((service) => service.state === 'succeeded').length
  const versions = [...new Set(services.map((service) => service.version).filter(Boolean))]
  return (
    <>
      <div className="runs-services">
        <ServiceStrip
          label={`${succeeded} of ${services.length} services succeeded`}
          cells={services.map((service) => ({
            status: service.state,
            title: `${service.service} ${service.version}: ${legStatusLabels[service.state]}`.replace('  ', ' '),
          }))}
        />
        <span className="runs-quiet runs-nowrap">{`${succeeded}/${services.length}`}</span>
      </div>
      {versions.length > 0 && (
        <div className="runs-quiet">
          {versions.slice(0, shownVersions).join(', ')}
          {versions.length > shownVersions && ` +${versions.length - shownVersions}`}
        </div>
      )}
    </>
  )
}

function WhatFailed({ run }: { run: RunSummary }) {
  const failures = run.summary.failures
  return (
    <div className="runs-fail">
      {failures.slice(0, shownFailures).map((failure) => (
        <div key={failure.leg_id}>
          {failure.service && <span className="mono">{failure.service}</span>} {failure.short}
        </div>
      ))}
      {failures.length > shownFailures && <div className="runs-quiet">{`+${failures.length - shownFailures} more`}</div>}
    </div>
  )
}

function duration(run: RunSummary): string {
  if (!run.started_at || !run.finished_at) return '—'
  return formatDuration(Date.parse(run.finished_at) - Date.parse(run.started_at))
}

/** The runs of a job or of everything, one row each. `showJob` is false on a job's own history. */
export function RunsTable({ runs, showJob }: { runs: RunSummary[]; showJob: boolean }) {
  const columns: DataColumn<RunSummary>[] = [
    {
      key: 'run',
      header: 'Run',
      render: (run) => (
        <>
          <Link to={`/runs/${run.id}`} className="mono">{`#${run.id}`}</Link>
          {run.restore_of != null && <div className="runs-quiet">{`Restore of #${run.restore_of}`}</div>}
        </>
      ),
    },
    { key: 'status', header: 'Status', render: (run) => <StatusPill status={run.state} /> },
  ]
  if (showJob) {
    columns.push({
      key: 'job',
      header: 'Job',
      render: (run) => <Link to={`/jobs/${run.job_source_id}`}>{run.job_name || run.job_id}</Link>,
    })
  }
  columns.push(
    {
      key: 'environment',
      header: 'Environment',
      className: 'runs-nowrap',
      render: (run) => (
        <>
          <span className="mono">{run.environment}</span> {run.label && <Tag>{run.label}</Tag>}
        </>
      ),
    },
    { key: 'services', header: 'Services', render: (run) => <Services run={run} /> },
    {
      key: 'started',
      header: 'Started',
      className: 'runs-nowrap',
      render: (run) => (
        <>
          {run.requested_by_name}
          <div className="runs-quiet">
            <Time iso={run.started_at ?? run.created_at} />
            {run.via && `, ${run.via}`}
          </div>
        </>
      ),
    },
    { key: 'duration', header: 'Duration', className: 'runs-nowrap', render: duration },
    { key: 'failed', header: 'What failed', className: 'runs-failed-cell', render: (run) => <WhatFailed run={run} /> },
  )
  return <DataTable label="Runs" columns={columns} rows={runs} rowKey={(run) => run.id} minWidth={showJob ? 1080 : 960} empty={null} />
}
