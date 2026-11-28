import type { ReactNode } from 'react'
import type { ConnectionHealth, RunnerInfo, SyncJobStatus, SystemHealth } from '../../api/types'
import {
  Card,
  DataTable,
  ErrorState,
  Facts,
  Loading,
  Note,
  StatusPill,
  Time,
  type DataColumn,
} from '../../ui'
import { durationWords } from '../queue/format'
import { RefreshNote } from '../queue/RefreshNote'
import { useSystemHealth } from './queries'
import './health.css'

function Section({ title, children }: { title: string; children: ReactNode }) {
  return (
    <section className="health-section" aria-label={title}>
      <Card flush>
        <h2>{title}</h2>
        {children}
      </Card>
    </section>
  )
}

const runnerColumns: DataColumn<RunnerInfo>[] = [
  { key: 'id', header: 'Runner', render: (runner) => <span className="mono">{runner.id}</span> },
  { key: 'version', header: 'Version', render: (runner) => runner.version },
  { key: 'busy', header: 'Busy', render: (runner) => `${runner.busy} of ${runner.concurrency}` },
  { key: 'started', header: 'Started', render: (runner) => <Time iso={runner.started_at} /> },
  { key: 'seen', header: 'Last seen', render: (runner) => <Time iso={runner.last_seen_at} /> },
]

interface SyncRow {
  key: string
  name: string
  checkedAt: string | null
  error: string
}

function syncRows(health: SystemHealth): SyncRow[] {
  const inventory = health.sync.inventory
  return [
    { key: 'inventory', name: 'Inventory', checkedAt: inventory.checked_at, error: inventory.error },
    ...health.sync.jobs.map((job: SyncJobStatus) => ({ key: `job-${job.id}`, name: job.title, checkedAt: job.checked_at, error: job.error })),
  ]
}

const syncColumns: DataColumn<SyncRow>[] = [
  { key: 'name', header: 'What', render: (row) => row.name },
  { key: 'checked', header: 'Checked', render: (row) => <Time iso={row.checkedAt} fallback="never" /> },
  { key: 'state', header: 'State', render: (row) => (row.error === '' ? 'OK' : <span className="health-failed">{row.error}</span>) },
]

const kindLabels: Record<ConnectionHealth['kind'], string> = { aws: 'AWS account', github: 'GitHub', slack: 'Slack' }

const connectionColumns: DataColumn<ConnectionHealth>[] = [
  { key: 'kind', header: 'Kind', render: (connection) => kindLabels[connection.kind] },
  {
    key: 'name',
    header: 'Name',
    render: (connection) => (
      <>
        {connection.name} {connection.failing && <StatusPill status="failed" />}
      </>
    ),
  },
  { key: 'ok', header: 'Last worked', render: (connection) => <Time iso={connection.last_ok_at} /> },
  {
    key: 'error',
    header: 'Last error',
    render: (connection) =>
      connection.last_error_at === null ? (
        <span className="queue-muted">None</span>
      ) : (
        <>
          <span className="mono">{connection.last_error_code}</span> <Time iso={connection.last_error_at} />
        </>
      ),
  },
]

interface CounterRow {
  name: string
  hour: number
  day: number
}

const counterColumns: DataColumn<CounterRow>[] = [
  { key: 'name', header: 'What', render: (row) => row.name },
  { key: 'hour', header: 'Last hour', render: (row) => row.hour },
  { key: 'day', header: 'Last day', render: (row) => row.day },
]

function Body({ health }: { health: SystemHealth }) {
  const counters: CounterRow[] = [
    { name: 'Expired leases', ...health.counters.lease_expired },
    { name: 'Log batches not stored', ...health.counters.log_persist_failed },
    { name: 'Sync failures', ...health.counters.sync_failed },
  ]
  const noRunner = `No runner has reported for ${durationWords(health.runner_gone_after_seconds)}.`
  return (
    <>
      {health.problems.length === 0 ? (
        <Note tone="success" className="health-problems">
          Everything is working
        </Note>
      ) : (
        <div className="health-problems">
          {health.problems.map((problem) => (
            <Note key={problem} tone="error">
              {problem}
            </Note>
          ))}
        </div>
      )}
      <div className="health-sections">
        <Section title="Runners">
          <DataTable label="Runners" columns={runnerColumns} rows={health.runners} rowKey={(runner) => runner.id} empty={<p className="health-note">{noRunner}</p>} />
        </Section>
        <Section title="Queue">
          <Facts
            className="health-facts"
            items={[
              { label: 'Queued legs', value: health.queue.queued_legs },
              { label: 'Oldest queued', value: <Time iso={health.queue.oldest_queued_at} fallback="Nothing queued" /> },
              { label: 'Active runs', value: health.queue.active_runs },
            ]}
          />
        </Section>
        <Section title="Sync">
          <DataTable label="Sync" columns={syncColumns} rows={syncRows(health)} rowKey={(row) => row.key} empty={null} />
        </Section>
        <Section title="Connections">
          <DataTable
            label="Connections"
            columns={connectionColumns}
            rows={health.connections}
            rowKey={(connection) => `${connection.kind}-${connection.name}`}
            empty={<p className="health-note">No connections are set up</p>}
          />
        </Section>
        <Section title="Notifications">
          <Facts
            className="health-facts"
            items={[
              { label: 'Pending', value: health.notifications.pending },
              { label: 'Failed in the last day', value: health.notifications.failed_last_day },
              { label: 'Oldest pending', value: <Time iso={health.notifications.oldest_pending_at} fallback="None pending" /> },
            ]}
          />
        </Section>
        <Section title="Recent failures">
          <DataTable label="Recent failures" columns={counterColumns} rows={counters} rowKey={(row) => row.name} empty={null} />
        </Section>
      </div>
    </>
  )
}

export function HealthPage() {
  const query = useSystemHealth()
  if (query.isPending) return <Loading />
  if (query.data === undefined) return <ErrorState error={query.error!} onRetry={() => void query.refetch()} />
  return (
    <>
      <RefreshNote failed={query.isError} fetchedAt={query.dataUpdatedAt} />
      <Body health={query.data} />
    </>
  )
}
