import { useEffect, useRef } from 'react'
import { isFinalRunState } from '../../api/types'
import type { Check, ConfigItem, Plan, PlanService, RunDetail } from '../../api/types'
import { Card, DataTable, Loading, Note, Tag, type DataColumn } from '../../ui'
import { failedChecks, fallbackItems } from './planModel'
import { useRunPlan } from './queries'
import './plan-panel.css'

const OPEN_STATES: RunDetail['state'][] = ['planned', 'waiting_approval']

function plural(count: number, one: string, many: string) {
  return `${count} ${count === 1 ? one : many}`
}

function summaryLine(plan: Plan) {
  const changing = plan.services.flatMap((service) => service.config).filter((item) => item.change === 'new' || item.change === 'changed').length
  const services = plural(plan.services.length, 'service', 'services')
  if (changing === 0) return `Plan: ${services}, no config files change`
  return `Plan: ${services}, ${changing} ${changing === 1 ? 'config file changes' : 'config files change'}`
}

function joinNames(names: string[]) {
  return names.length > 1 ? `${names.slice(0, -1).join(', ')} and ${names[names.length - 1]}` : (names[0] ?? '')
}

interface FallbackGroup {
  branch: string | undefined
  version: string
  services: string[]
}

// One group per (fallback branch, version) so each sentence names the right branch and version.
function groupFallbacks(found: ReturnType<typeof fallbackItems>): FallbackGroup[] {
  const groups = new Map<string, FallbackGroup>()
  for (const { service, item } of found) {
    const key = `${item.branch ?? ''}\u0000${service.version}`
    const group = groups.get(key) ?? { branch: item.branch, version: service.version, services: [] }
    if (!group.services.includes(service.name)) group.services.push(service.name)
    groups.set(key, group)
  }
  return [...groups.values()]
}

function fallbackSentence(group: FallbackGroup) {
  const from = group.branch ? `branch ${group.branch}` : 'another branch'
  return `Env files for ${joinNames(group.services)} come from ${from} because branch v${group.version} does not exist`
}

function CheckList({ checks }: { checks: Check[] }) {
  return (
    <ul className="plan-checks">
      {checks.map((check) => (
        <li key={`${check.name}/${check.service}`}>
          <strong>{check.name}</strong> for <span className="plan-mono">{check.service}</span>: {check.detail}
        </li>
      ))}
    </ul>
  )
}

function KeyList({ label, keys }: { label: string; keys: string[] }) {
  if (keys.length === 0) return null
  return (
    <div className="plan-keys">
      <span className="plan-keys-label">{label}</span>
      {keys.map((key) => (
        <Tag key={key} className="plan-mono">
          {key}
        </Tag>
      ))}
    </div>
  )
}

function ConfigEntry({ item }: { item: ConfigItem }) {
  const keys = item.kind === 'env' && item.change === 'changed' ? item.keys : null
  return (
    <li className="plan-config-item">
      <span className="plan-config-head">
        <span className="plan-mono">{item.name}</span>
        <Tag data-change={item.change}>{item.change === 'unknown' ? 'Unknown' : item.change}</Tag>
      </span>
      {keys && (
        <>
          <KeyList label="Added" keys={keys.added} />
          <KeyList label="Removed" keys={keys.removed} />
          <KeyList label="Changed" keys={keys.changed} />
        </>
      )}
      {item.not_restored && <Note tone="warning">{item.not_restored}</Note>}
    </li>
  )
}

function columnsFor(restore: boolean): DataColumn<PlanService>[] {
  return [
    { key: 'service', header: 'Service', render: (service) => <span className="plan-mono">{service.name}</span> },
    {
      key: 'from',
      header: restore ? 'Now' : 'From',
      render: (service) =>
        !service.previous_version ? (
          <span className="plan-old">unknown</span>
        ) : service.previous_version === service.version ? (
          <span className="plan-old">unchanged</span>
        ) : (
          <span className="plan-old plan-mono">{service.previous_version}</span>
        ),
    },
    {
      key: 'to',
      header: restore ? 'Restored to' : 'To',
      render: (service) => <span className="plan-mono">{service.version}</span>,
    },
    {
      key: 'task',
      header: 'Task definition',
      render: (service) => (
        <span className="plan-mono">
          {service.previous_task_definition}
          {service.target_task_definition && <> {'→'} {service.target_task_definition}</>}
        </span>
      ),
    },
    {
      key: 'config',
      header: 'Config',
      render: (service) =>
        service.config.length === 0 ? (
          <span className="plan-old">none</span>
        ) : (
          <ul className="plan-config">
            {service.config.map((item) => (
              <ConfigEntry key={`${item.kind}/${item.name}`} item={item} />
            ))}
          </ul>
        ),
    },
  ]
}

export function PlanPanel({ run }: { run: RunDetail }) {
  const { data: plan, error, isPending, refetch } = useRunPlan(run.id)

  // A 404 means "not planned yet". When the run's state moves past resolving (a state event on an open page),
  // ask again, so the plan appears without a reload, also for a person who cannot start the run.
  // The ref holds the state the run was in when the plan was last asked for. It moves only when we ask again, so a
  // state event that arrives while the first request is in flight is still seen once that request answers 404.
  const stateAtLastAsk = useRef(run.state)
  const notPlanned = error?.status === 404
  useEffect(() => {
    if (!notPlanned || stateAtLastAsk.current === run.state) return
    if (run.state === 'resolving') return
    stateAtLastAsk.current = run.state
    void refetch()
  }, [run.state, notPlanned, refetch])

  if (isPending || notPlanned) {
    return (
      <Card>
        <h2 className="plan-title">Plan</h2>
        {isPending ? (
          <Loading />
        ) : isFinalRunState(run.state) ? (
          <p className="plan-old">This run has no plan.</p>
        ) : (
          <p className="plan-old">Loading plan. It appears once the run has been planned.</p>
        )}
      </Card>
    )
  }
  if (error || !plan) {
    return (
      <Card>
        <h2 className="plan-title">Plan</h2>
        <Note tone="error">The plan could not be loaded: {error?.message}</Note>
      </Card>
    )
  }

  const restore = run.kind === 'restore' || plan.restore_of != null
  const failed = failedChecks(plan)
  const fallbacks = fallbackItems(plan)
  const fallbackGroups = groupFallbacks(fallbacks)
  const unverified = plan.checks.filter((check) => check.status === 'not_verified')

  return (
    <Card className="plan-panel">
      {/* Outside the details: in a failed run the card is closed and this is the reason it failed. */}
      {failed.length > 0 && (
        <Note tone="error" className="plan-failed">
          <p>
            <strong>This run cannot start.</strong> {plural(failed.length, 'check failed', 'checks failed')}.
          </p>
          <CheckList checks={failed} />
        </Note>
      )}
      <details open={OPEN_STATES.includes(run.state)}>
        <summary>
          <span className="plan-title">Plan</span>
          <span className="plan-summary">{summaryLine(plan)}</span>
        </summary>
        <div className="plan-body">
          {fallbackGroups.length > 0 && (
            <Note tone="warning">
              {fallbackGroups.map((group) => (
                <p key={`${group.branch}/${group.version}`}>{fallbackSentence(group)}</p>
              ))}
            </Note>
          )}
          {unverified.length > 0 && (
            <Note tone="info">
              {plural(unverified.length, 'check', 'checks')} could not be verified:{' '}
              {unverified.map((check) => `${check.name} for ${check.service}`).join(', ')}.
            </Note>
          )}
          <DataTable
            label="Plan"
            columns={columnsFor(restore)}
            rows={plan.services}
            rowKey={(service) => service.name}
            empty={<p className="plan-old">The plan changes no services.</p>}
            minWidth={640}
          />
        </div>
      </details>
    </Card>
  )
}
