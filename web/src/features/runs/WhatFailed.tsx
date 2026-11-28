// The panel under a run that did not finish well: what failed, in which step, why, and a way to its log.
import { Link } from 'react-router-dom'
import type { RunDetail } from '../../api/types'
import { Button } from '../../ui'
import { servicePath } from '../service/servicePath'
import type { ConsoleScope } from './scope'
import { wholeRun } from './scope'

export interface WhatFailedProps {
  run: RunDetail
  /** Sets the console's scope (the log of one service or step). */
  onShowLog(scope: ConsoleScope): void
}

function scopeOf(service: string, step: string): ConsoleScope {
  if (service) return { kind: 'service', service }
  if (step) return { kind: 'step', step }
  return wholeRun
}

/** "deploy and verify", "a, b and c". */
function joined(words: string[]): string {
  if (words.length <= 1) return words.join('')
  return `${words.slice(0, -1).join(', ')} and ${words[words.length - 1]}`
}

export function WhatFailed({ run, onShowLog }: WhatFailedProps) {
  const failures = run.summary.failures
  const skippedByService = new Map<string, string[]>()
  for (const leg of run.legs) {
    if (leg.state !== 'skipped' || leg.kind !== 'step') continue
    skippedByService.set(leg.service, [...(skippedByService.get(leg.service) ?? []), leg.step])
  }
  if (failures.length === 0 && skippedByService.size === 0) return null

  return (
    <section className="run-fail" aria-labelledby="run-fail-title">
      <h2 id="run-fail-title" className="run-fail-title">
        What failed
      </h2>
      <div className="run-fail-scroll">
        <table>
          <thead>
            <tr>
              <th scope="col">Failed</th>
              <th scope="col">In step</th>
              <th scope="col">Why</th>
              <th scope="col">Log</th>
            </tr>
          </thead>
          <tbody>
            {failures.map((failure) => (
              <tr key={failure.leg_id}>
                <td data-label="Failed">
                  {failure.service ? (
                    <Link className="mono run-fail-what" to={servicePath(failure.service)}>
                      {failure.service}
                    </Link>
                  ) : (
                    <span className="mono run-fail-what">{failure.step || 'Run'}</span>
                  )}
                  <div className="run-fail-sub">{failure.state === 'unknown' ? 'Outcome not confirmed' : failure.service ? 'Service' : 'Step'}</div>
                </td>
                <td className="mono" data-label="In step">{failure.step || failure.kind}</td>
                <td className="run-fail-why" data-label="Why">{failure.detail || failure.short}</td>
                <td>
                  <Button size="sm" onClick={() => onShowLog(scopeOf(failure.service, failure.step))}>
                    Show its log
                  </Button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {Array.from(skippedByService, ([service, steps]) => (
        <p key={service} className="run-fail-skipped">
          <Link to={servicePath(service)}>{service}</Link>: {joined(steps)} {steps.length === 1 ? 'was' : 'were'} skipped
        </p>
      ))}
    </section>
  )
}
