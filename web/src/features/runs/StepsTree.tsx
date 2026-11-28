// The steps and services of a run as a list of buttons. The chosen item is the console's scope.
// Planning legs (resolve, revalidate) have no step: they are their own items before the steps, with the
// scope { kind: 'step', step: ':resolve' | ':revalidate' } (a colon cannot be in a real step name).
import { useMemo } from 'react'
import { Link } from 'react-router-dom'
import type { LegState, LogLine, RunDetail, RunLeg } from '../../api/types'
import { statusLabel, statusShape, statusTone } from '../../ui'
import { combinedState, needsAttention, planningStep } from './consoleModel'
import { servicePath } from '../service/servicePath'
import { sameScope, wholeRun, type ConsoleScope } from './scope'
import './console.css'

interface ServiceItem {
  key: string
  service: string
  version: string
  state: LegState
  attempt: number
}

interface StepItem {
  key: string
  scope: ConsoleScope
  name: string
  meta: string
  state: LegState
  attempt: number
  services: ServiceItem[]
}

const maxAttempt = (legs: RunLeg[]) => legs.reduce((highest, leg) => Math.max(highest, leg.attempt), 0)

function buildItems(run: RunDetail): StepItem[] {
  const items: StepItem[] = []
  for (const kind of ['resolve', 'revalidate'] as const) {
    const legs = run.legs.filter((leg) => leg.kind === kind)
    if (legs.length === 0) continue
    items.push({
      key: `:${kind}`,
      scope: { kind: 'step', step: planningStep(kind) },
      name: kind === 'resolve' ? 'Resolve' : 'Revalidate',
      meta: '',
      state: combinedState(legs.map((leg) => leg.state)),
      attempt: maxAttempt(legs),
      services: [],
    })
  }
  const orderedSteps = [...run.steps].sort((a, b) => a.position - b.position)
  for (const step of orderedSteps) {
    const legs = run.legs.filter((leg) => leg.kind === 'step' && leg.step === step.name)
    const serviceLegs = legs.filter((leg) => leg.service !== '')
    const services = serviceLegs
      .map<ServiceItem>((leg) => ({ key: `${step.name}/${leg.service}`, service: leg.service, version: leg.version, state: leg.state, attempt: leg.attempt }))
      // Failed and unknown first; the sort is stable, so the rest keep their order.
      .sort((a, b) => Number(needsAttention(b.state)) - Number(needsAttention(a.state)))
    const done = serviceLegs.filter((leg) => leg.state === 'succeeded').length
    items.push({
      key: step.name,
      scope: { kind: 'step', step: step.name },
      name: step.name,
      meta: serviceLegs.length > 0 ? `${done}/${serviceLegs.length}` : step.use,
      state: combinedState(legs.map((leg) => leg.state)),
      attempt: maxAttempt(legs),
      services,
    })
  }
  return items
}

function Mark({ state }: { state: LegState }) {
  return (
    <span className="tn-mark" data-tone={statusTone(state)}>
      <span className="ui-shape" data-shape={statusShape(state)} aria-hidden="true" />
    </span>
  )
}

export function StepsTree({
  run,
  lines,
  scope,
  onSelect,
}: {
  run: RunDetail
  lines: LogLine[]
  scope: ConsoleScope
  onSelect(scope: ConsoleScope): void
}) {
  const items = useMemo(() => buildItems(run), [run])
  // A service listed under several steps is one scope; only its first row is marked as the current item.
  const currentServiceKey =
    scope.kind === 'service' ? items.flatMap((item) => item.services).find((service) => service.service === scope.service)?.key : undefined
  const choose = (next: ConsoleScope) => () => onSelect(next)

  return (
    <nav className="tree" aria-label="Steps and services">
      <div className="tree-h">Steps and services</div>
      <div className="tree-list">
        <button type="button" className="tn" aria-current={sameScope(scope, wholeRun) ? 'true' : undefined} onClick={choose(wholeRun)}>
          <span className="tn-name">Whole run</span>
          <span className="tn-meta">{lines.length}</span>
        </button>
        {items.map((item) => (
          <div key={item.key} className="tn-group">
            <button type="button" className="tn" aria-current={sameScope(scope, item.scope) ? 'true' : undefined} onClick={choose(item.scope)}>
              <Mark state={item.state} />
              <span className="tn-name">{item.name}</span>
              {item.attempt > 1 && <span className="tn-attempt">attempt {item.attempt}</span>}
              {item.meta !== '' && <span className="tn-meta">{item.meta}</span>}
              <span className="con-sr">{statusLabel(item.state)}</span>
            </button>
            {item.services.map((service) => {
              const serviceScope: ConsoleScope = { kind: 'service', service: service.service }
              return (
                <div key={service.key} className="tn-svc-row">
                  <button
                    type="button"
                    className="tn tn-svc"
                    aria-current={service.key === currentServiceKey ? 'true' : undefined}
                    onClick={choose(serviceScope)}
                  >
                    <Mark state={service.state} />
                    <span className="tn-name">{service.service}</span>
                    {service.attempt > 1 && <span className="tn-attempt">attempt {service.attempt}</span>}
                    <span className="tn-meta">{service.version}</span>
                    <span className="con-sr">{statusLabel(service.state)}</span>
                  </button>
                  <Link className="tn-open" to={servicePath(service.service)} aria-label={`${service.service} service page`}>
                    Page
                  </Link>
                </div>
              )
            })}
          </div>
        ))}
      </div>
    </nav>
  )
}
