import { Link } from 'react-router-dom'
import type { QueueLock, QueueRun, QueueView } from '../../api/types'
import { Card, DataTable, ErrorState, Loading, Note, PageHeader, StatusPill, Time, formatDuration, type DataColumn } from '../../ui'
import { durationWords, plural } from './format'
import { RefreshNote } from './RefreshNote'
import { useQueue } from './queries'
import './queue.css'

const resourceWidth = 48

/** Cuts a long value in the middle so both its start and end stay readable. */
function shortenMiddle(value: string, width: number): string {
  if (value.length <= width) return value
  const keep = width - 1
  const head = Math.ceil(keep / 2)
  return `${value.slice(0, head)}…${value.slice(value.length - (keep - head))}`
}

function RunLink({ id }: { id: number }) {
  return <Link to={`/runs/${id}`}>#{id}</Link>
}

function WaitsFor({ run }: { run: QueueRun }) {
  if (run.blocked_by_run_id !== null) {
    return (
      <>
        Waiting for run <RunLink id={run.blocked_by_run_id} />
      </>
    )
  }
  if (run.wait_reason === '') return <span className="queue-muted">Nothing</span>
  return <>{run.wait_reason.charAt(0).toUpperCase() + run.wait_reason.slice(1)}</>
}

const runColumns: DataColumn<QueueRun>[] = [
  { key: 'run', header: 'Run', render: (run) => <RunLink id={run.id} /> },
  { key: 'job', header: 'Job', render: (run) => run.job_name || `Job ${run.job_id}` },
  { key: 'environment', header: 'Environment', render: (run) => <span className="mono">{run.environment}</span> },
  { key: 'state', header: 'State', render: (run) => <StatusPill status={run.state} /> },
  { key: 'waits', header: 'Waits for', render: (run) => <WaitsFor run={run} /> },
  { key: 'waited', header: 'Waited', render: (run) => formatDuration(run.waited_seconds * 1000) },
]

const lockColumns: DataColumn<QueueLock>[] = [
  {
    key: 'resource',
    header: 'Resource',
    render: (lock) => (
      <span className="mono queue-resource" title={lock.resource}>
        {shortenMiddle(lock.resource, resourceWidth)}
      </span>
    ),
  },
  { key: 'run', header: 'Held by', render: (lock) => <RunLink id={lock.run_id} /> },
  { key: 'since', header: 'Since', render: (lock) => <Time iso={lock.acquired_at} /> },
]

function Capacity({ queue }: { queue: QueueView }) {
  if (queue.runners.length === 0) {
    return (
      <Note tone="error" className="queue-stale">
        No runner has reported for {durationWords(queue.runner_gone_after_seconds)}. Queued runs will not start until one does.
      </Note>
    )
  }
  const slots = queue.runners.reduce((sum, runner) => sum + runner.concurrency, 0)
  const busy = queue.runners.reduce((sum, runner) => sum + runner.busy, 0)
  return (
    <p className="queue-capacity">
      {plural(queue.runners.length, 'runner')}, {busy} of {slots} slots busy
    </p>
  )
}

/** `asOf` is when the numbers were fetched, so the wait is measured against that moment. */
function Backlog({ queue, asOf }: { queue: QueueView; asOf: number }) {
  if (queue.queued_legs === 0 || queue.oldest_queued_at === null) return <p className="queue-backlog">Nothing queued</p>
  const waited = Math.max(0, asOf - Date.parse(queue.oldest_queued_at))
  return (
    <p className="queue-backlog">
      {plural(queue.queued_legs, 'leg')} queued, oldest waiting {formatDuration(waited)}
    </p>
  )
}

export function QueuePage() {
  const query = useQueue()
  const header = <PageHeader title="Queue" lead="Runs that are active or waiting, and the locks they hold. Refreshed every 5 seconds." />
  if (query.isPending) {
    return (
      <>
        {header}
        <Loading />
      </>
    )
  }
  if (query.data === undefined) {
    return (
      <>
        {header}
        <ErrorState error={query.error!} onRetry={() => void query.refetch()} />
      </>
    )
  }
  const queue = query.data
  return (
    <>
      {header}
      <RefreshNote failed={query.isError} fetchedAt={query.dataUpdatedAt} />
      <Capacity queue={queue} />
      <Backlog queue={queue} asOf={query.dataUpdatedAt} />
      <section className="queue-section" aria-label="Runs">
        <Card flush>
          <DataTable
            label="Active and waiting runs"
            columns={runColumns}
            rows={queue.runs}
            rowKey={(run) => run.id}
            empty={<p className="queue-empty">No active or waiting runs</p>}
          />
        </Card>
      </section>
      <section className="queue-section" aria-label="Held locks">
        <Card flush>
          <DataTable
            label="Locks"
            columns={lockColumns}
            rows={queue.locks}
            rowKey={(lock) => lock.resource}
            empty={<p className="queue-empty">No locks are held</p>}
          />
        </Card>
      </section>
    </>
  )
}
