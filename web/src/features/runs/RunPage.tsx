// One run: where it stands, what can be done about it, its plan, its steps and its console.
import { useState, type ReactNode } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useCan } from '../../api/hooks'
import type { Target } from '../../auth/permissions'
import { ApiError } from '../../api/error'
import type { RunDetail } from '../../api/types'
import { isFinalRunState } from '../../api/types'
import { Button, Card, ErrorState, Loading, Note, PageHeader, StatusPill, Time } from '../../ui'
import { FreezeBanner } from '../freezes/FreezeBanner'
import { Console } from './Console'
import { PlanPanel } from './PlanPanel'
import { Reconcile } from './Reconcile'
import { RunActions } from './RunActions'
import { RunFacts } from './RunFacts'
import { StepsTree } from './StepsTree'
import { WhatFailed } from './WhatFailed'
import { plural, serviceProgress, updatedCount } from './runModel'
import { useConsoleScope } from './scope'
import { useRunLive } from './useRunLive'
import './run-page.css'

/** The server's wait reason, with "run #N" linking to that run. */
function WaitReason({ text }: { text: string }) {
  const parts = text.split(/(run #\d+)/g)
  return (
    <>
      {parts.map((part, index) => {
        const match = /^run #(\d+)$/.exec(part)
        return match ? (
          <Link key={index} to={`/runs/${match[1]}`}>
            {part}
          </Link>
        ) : (
          part
        )
      })}
    </>
  )
}

const upperFirst = (text: string) => text.charAt(0).toUpperCase() + text.slice(1)

// The reasons the engine writes when nothing more specific is known (internal/engine/advance.go defaultEndReason).
const defaultEndReasons = {
  cancelled: "cancelled at a person's request",
  invalidated: 'the targets changed since the plan was made, so nothing was run',
}

/** The server's reason, capitalised, when it says more than the state already does. */
function extraReason(reason: string, fixed: string): string {
  return reason && reason !== fixed ? upperFirst(reason) : ''
}

function stateSentence(run: RunDetail): ReactNode {
  const total = serviceProgress(run).length
  const updated = updatedCount(run)
  switch (run.state) {
    case 'resolving':
      return 'Planning: reading current versions and config'
    case 'planned':
      return 'Review the plan, then start'
    case 'waiting_approval':
      return (
        <>
          Waiting for approval, expires <Time iso={run.approval?.expires_at} fallback="at an unknown time" />
        </>
      )
    case 'queued':
      return run.wait_reason ? <WaitReason text={run.wait_reason} /> : 'Waiting for its turn'
    case 'running':
      return `${updated} of ${plural(total, 'service')} updated so far`
    case 'cancelling':
      return 'Cancelling: waiting for services already rolling to finish'
    case 'needs_reconciliation': {
      const unconfirmed = run.summary.counts.unknown
      return `${plural(unconfirmed, 'outcome')} could not be confirmed. Its services stay locked until they are resolved`
    }
    case 'succeeded':
      return `All ${plural(total, 'service')} updated`
    case 'partially_succeeded':
    case 'failed':
      return `${updated} of ${plural(total, 'service')} updated`
    case 'cancelled': {
      // The API does not send who cancelled yet; cancelled_by_name is read when it does.
      const who = run.cancelled_by_name
      const detail = extraReason(run.end_reason, defaultEndReasons.cancelled)
      return `Cancelled${who ? ` by ${who}` : ''}.${detail ? ` ${detail}.` : ''} ${plural(updated, 'service')} had already been updated`
    }
    case 'invalidated': {
      const detail = extraReason(run.end_reason, defaultEndReasons.invalidated)
      return `The targets changed after this plan was made${detail ? `. ${detail}` : ''}`
    }
    case 'rejected':
      // The engine's reason already says who: "rejected by <name>".
      return (
        <>
          {upperFirst(run.end_reason || 'rejected')} <Time iso={run.state_changed_at} />
        </>
      )
    case 'expired':
      return (
        <>
          Expired{run.end_reason ? `: ${run.end_reason}` : ''} <Time iso={run.state_changed_at} />
        </>
      )
  }
}

const planningStates: RunDetail['state'][] = ['resolving', 'planned', 'waiting_approval', 'queued']

export function RunPage() {
  const { id = '' } = useParams()
  // Keyed by the run so moving to another run (after Restore or Plan again) starts with a clean page.
  return <RunView key={id} id={Number(id)} />
}

function RunView({ id }: { id: number }) {
  const [scope, setScope] = useConsoleScope()
  const live = useRunWithLogAccess(id)
  const run = live.run

  if (live.error && !run) {
    if (live.error instanceof ApiError && live.error.status === 404) {
      return (
        <>
          <PageHeader title="Run not found" lead="There is no run at this address." />
          <Card>
            <Link to="/runs">Go to all runs</Link>
          </Card>
        </>
      )
    }
    return <ErrorState error={live.error} onRetry={live.reload} />
  }
  if (!run) return <Loading />

  const failedStates: RunDetail['state'][] = ['partially_succeeded', 'failed']
  return (
    <div className="run-page">
      <PageHeader
        crumbs={
          <>
            <Link to="/jobs">Jobs</Link>
            <span aria-hidden="true">/</span>
            <Link to={`/jobs/${run.job_source_id}`}>{run.job_name || run.job_id}</Link>
            <span aria-hidden="true">/</span>
            <span>Run #{run.id}</span>
          </>
        }
        title={
          <>
            Run #{run.id} <StatusPill status={run.state} />
          </>
        }
        lead={<span data-testid="run-lead">{stateSentence(run)}</span>}
      />
      {live.error && !isFinalRunState(run.state) && (
        <Note tone="warning" data-testid="live-stopped">
          Live updates stopped: {live.error.message}. The state shown is the last known state, and it may be out of date.{' '}
          <Button size="sm" onClick={live.retry}>
            Retry
          </Button>
        </Note>
      )}
      <RunActions run={run} reload={live.reload} />
      <RunFacts run={run} />
      {planningStates.includes(run.state) && <FreezeBanner environment={run.environment} label={run.label} />}
      {run.state === 'needs_reconciliation' && <Reconcile run={run} onShowLog={setScope} />}
      {(failedStates.includes(run.state) || run.state === 'cancelled') && <WhatFailed run={run} onShowLog={setScope} />}
      <PlanPanel run={run} />
      <div className="run-work">
        <StepsTree run={run} lines={live.lines} scope={scope} onSelect={setScope} />
        <Console run={run} live={live} scope={scope} />
      </div>
    </div>
  )
}

// Log access depends on the run's label, environment and folder, which are known once the run has loaded; until
// then the person's general right decides (a person whose logs are scoped gets them once the run is known).
function useRunWithLogAccess(id: number) {
  const can = useCan()
  const [target, setTarget] = useState<Target>({})
  const live = useRunLive(id, { logs: can('view_logs', target) })
  const run = live.run
  if (run && (target.label !== run.label || target.environment !== run.environment || target.folderId !== run.folder_id)) {
    setTarget({ label: run.label, environment: run.environment, folderId: run.folder_id })
  }
  return live
}
