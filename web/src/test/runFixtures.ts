// Run fixtures for the mock API: one run for every state the run screens must handle, with legs, steps,
// plans and log lines. Built by small helpers so a test can build another run the same way.
//
// Run ids (job, environment, state):
//   101  job 3 (portal)   preprod-01  planned, with a diff: a changed env file that fell back to the dev branch
//   102  job 2 (billing)  hetzner-01  planned, with a failed check (Start must be absent)
//   103  job 1 (backend)  prod-01     waiting_approval, asked by Yael Barak (id 6)
//   104  job 3 (portal)   hetzner-01  running (asked by Omer Katz, id 3)
//   105  job 3 (portal)   hetzner-02  partially_succeeded: billing failed, its later legs skipped
//   106  job 3 (portal)   preprod-01  needs_reconciliation: two unknown legs, locks held
//   107  job 2 (billing)  hetzner-02  invalidated
//   108  job 1 (backend)  hetzner-01  succeeded restore of run 109
//   109  job 1 (backend)  hetzner-01  succeeded deploy
import type {
  Approval,
  ApiLogLine,
  Check,
  ConfigItem,
  LegKind,
  LegState,
  Plan,
  PlanService,
  RunDetail,
  RunLeg,
  RunServiceChip,
  RunState,
  RunStep,
  RunSummary,
  RunSummaryCounts,
  Selection,
} from '../api/types'
import { serviceProgress } from '../features/runs/runModel'

export const mockNow = '2026-11-27T12:00:00Z'

export const runIds = {
  planned: 101,
  plannedFailedCheck: 102,
  waitingApproval: 103,
  running: 104,
  partiallySucceeded: 105,
  needsReconciliation: 106,
  invalidated: 107,
  restoreSucceeded: 108,
  succeeded: 109,
} as const

const legStates: LegState[] = ['pending', 'queued', 'claimed', 'running', 'succeeded', 'failed', 'skipped', 'cancelled', 'unknown']

/** Counts for every leg state (zero included) and the failed and unknown legs, as the server computes them. */
export function summarizeLegs(legs: RunLeg[]): RunSummaryCounts {
  const counts = Object.fromEntries(legStates.map((state) => [state, 0])) as RunSummaryCounts['counts']
  const failures: RunSummaryCounts['failures'] = []
  for (const leg of legs) {
    counts[leg.state] += 1
    if (leg.state === 'failed' || leg.state === 'unknown') {
      failures.push({
        leg_id: leg.id,
        service: leg.service,
        step: leg.step,
        kind: leg.kind,
        state: leg.state,
        short: leg.failure_short,
        detail: leg.failure_detail,
      })
    }
  }
  return { counts, failures }
}

// The list row's services are the run page's service progress, as the server computes them with the same rule.
function serviceChips(run: RunDetail): RunServiceChip[] {
  return serviceProgress(run).map(({ service, version, status }) => ({ service, version, state: status }))
}

/** The list row of a stored run (the list adds services and the summary; the detail has more). */
export function runRow(run: RunDetail): RunSummary {
  return {
    id: run.id,
    kind: run.kind,
    job_source_id: run.job_source_id,
    job_id: run.job_id,
    environment: run.environment,
    label: run.label,
    state: run.state,
    wait_reason: run.wait_reason,
    end_reason: run.end_reason,
    manifest_id: run.manifest_id,
    requested_by: run.requested_by,
    via: run.via,
    created_at: run.created_at,
    state_changed_at: run.state_changed_at,
    started_at: run.started_at,
    finished_at: run.finished_at,
    job_name: run.job_name,
    requested_by_name: run.requested_by_name,
    restore_of: run.restore_of ?? null,
    services: serviceChips(run),
    summary: summarizeLegs(run.legs),
  }
}

// ---- The jobs and environments the runs refer to ------------------------------------------------

export interface RunJob {
  sourceId: number
  jobId: string
  title: string
  folderId: number
  groups: { id: string; services: string[] }[]
  approvalLabels: string[]
}

/** The first three jobs of the job fixtures, as far as runs need them. */
export const runJobs: Record<number, RunJob> = {
  1: { sourceId: 1, jobId: 'deploy-backend', title: 'Backend and microservices', folderId: 1, groups: [{ id: 'backends', services: ['backend-api'] }], approvalLabels: ['prod'] },
  2: { sourceId: 2, jobId: 'deploy-billing', title: 'Billing service only', folderId: 1, groups: [{ id: 'billing', services: ['billing'] }], approvalLabels: [] },
  3: { sourceId: 3, jobId: 'deploy-portal', title: 'Portal services', folderId: 2, groups: [{ id: 'portal', services: ['backend-api', 'billing'] }], approvalLabels: [] },
}

export const environmentLabels: Record<string, { label: string; account: string }> = {
  'hetzner-01': { label: 'dev', account: '111111111111' },
  'hetzner-02': { label: 'dev', account: '111111111111' },
  'hetzner-03': { label: 'dev', account: '111111111111' },
  'preprod-01': { label: 'preprod', account: '222222222222' },
  'prod-01': { label: 'prod', account: '333333333333' },
}

/** An id the mock gives to something that has no real hash: 64 hex digits that differ per number. */
export function fakeHash(seed: number): string {
  return seed.toString(16).padStart(8, '0').repeat(8)
}

// ---- Building a run ---------------------------------------------------------------------------------

export interface LegSpec {
  /** A step name, or "resolve" or "revalidate". */
  step: string
  service?: string
  state: LegState
  short?: string
  detail?: string
  attempt?: number
}

export interface RunSpec {
  id: number
  job: number
  /** For a job that is not one of runJobs (for example one a test registered). */
  jobInfo?: RunJob
  environment: string
  state: RunState
  /** service -> version. */
  services: Record<string, string>
  legs: LegSpec[]
  requestedBy: { id: number; name: string }
  createdAt: string
  startedAt?: string | null
  finishedAt?: string | null
  waitReason?: string
  endReason?: string
  locks?: string[]
  kind?: 'deploy' | 'restore'
  restoreOf?: number | null
  autoStart?: boolean
  cancelRequested?: boolean
  steps?: { name: string; use: string }[]
  selection?: Selection
  approval?: Approval | null
}

const defaultSteps = [
  { name: 'copy-config', use: 'copy-config' },
  { name: 'deploy', use: 'deploy-ecs' },
]

const addSeconds = (iso: string, seconds: number) => new Date(Date.parse(iso) + seconds * 1000).toISOString()

/** A run in the shape of GET /api/v1/runs/{id}. Leg and step ids are the run id times 100 plus their position. */
export function buildRun(spec: RunSpec): RunDetail {
  const job = spec.jobInfo ?? runJobs[spec.job]
  if (!job) {
    throw new Error(`no job ${spec.job} for the run fixtures`)
  }
  const { label, account } = environmentLabels[spec.environment] ?? { label: '', account: '' }
  const stepDefinitions = spec.steps ?? defaultSteps
  const groupId = spec.kind === 'restore' ? 'restore' : (job.groups[0]?.id ?? '')
  const steps: RunStep[] = stepDefinitions.map((step, index) => ({
    id: spec.id * 100 + index + 1,
    name: step.name,
    use: step.use,
    group: groupId,
    position: index + 1,
    max_parallel: 0,
  }))
  const begin = spec.startedAt ?? spec.createdAt
  const legs: RunLeg[] = spec.legs.map((leg, index) => {
    const step = steps.find((candidate) => candidate.name === leg.step)
    const kind: LegKind = leg.step === 'resolve' || leg.step === 'revalidate' ? leg.step : 'step'
    const started = ['pending', 'queued', 'skipped', 'cancelled'].includes(leg.state) ? null : addSeconds(begin, index * 20)
    const finished = ['succeeded', 'failed', 'unknown'].includes(leg.state) && started ? addSeconds(started, 15) : null
    const service = leg.service ?? ''
    return {
      id: spec.id * 100 + index + 1,
      step_id: step ? step.id : null,
      step: step ? step.name : '',
      kind,
      service,
      version: spec.services[service] ?? '',
      state: leg.state,
      attempt: leg.attempt ?? (started ? 1 : 0),
      cancel_requested: false,
      failure_short: leg.short ?? '',
      failure_detail: leg.detail ?? '',
      queued_at: leg.state === 'pending' ? null : addSeconds(begin, index * 20 - 2),
      started_at: started,
      finished_at: finished,
    }
  })
  const finalStates: RunState[] = ['succeeded', 'partially_succeeded', 'failed', 'cancelled', 'rejected', 'expired', 'invalidated']
  const isFinal = finalStates.includes(spec.state)
  return {
    id: spec.id,
    kind: spec.kind ?? 'deploy',
    job_source_id: job.sourceId,
    job_id: job.jobId,
    environment: spec.environment,
    label,
    state: spec.state,
    wait_reason: spec.waitReason ?? '',
    end_reason: spec.endReason ?? '',
    manifest_id: fakeHash(spec.id),
    requested_by: spec.requestedBy.id,
    via: 'web',
    created_at: spec.createdAt,
    state_changed_at: spec.finishedAt ?? spec.startedAt ?? spec.createdAt,
    started_at: spec.startedAt ?? null,
    finished_at: spec.finishedAt ?? (isFinal ? addSeconds(begin, 300) : null),
    job_name: job.title,
    requested_by_name: spec.requestedBy.name,
    restore_of: spec.restoreOf ?? null,
    aws_account: account,
    folder_id: job.folderId,
    inventory_commit: 'a1b2c3d4e5f60718293a4b5c6d7e8f9012345678',
    job_commit: `${spec.job}`.repeat(40),
    approval_required: job.approvalLabels.includes(label),
    auto_start: spec.autoStart ?? false,
    steps,
    cancel_requested: spec.cancelRequested ?? false,
    legs,
    locks: spec.locks ?? [],
    summary: summarizeLegs(legs),
    selection: spec.selection,
    approval: spec.approval ?? null,
  }
}

/** The legs of a plain run: resolve and revalidate, then each step for each service (steps in order). */
export function standardLegs(
  services: string[],
  states: { resolve?: LegState; revalidate?: LegState; perStep?: Record<string, Record<string, LegState>> },
  stepNames: string[] = ['copy-config', 'deploy'],
): LegSpec[] {
  const legs: LegSpec[] = [
    { step: 'resolve', state: states.resolve ?? 'succeeded' },
    { step: 'revalidate', state: states.revalidate ?? 'pending' },
  ]
  for (const step of stepNames) {
    for (const service of services) {
      legs.push({ step, service, state: states.perStep?.[step]?.[service] ?? 'pending' })
    }
  }
  return legs
}

// ---- Plans -------------------------------------------------------------------------------------------

function envItem(service: string, environment: string, change: ConfigItem['change'], extra: Partial<ConfigItem> = {}): ConfigItem {
  return {
    kind: 'env',
    name: `${service}.env`,
    source: `example-org/env-config-repo/${environment}/${service}/${service}.env`,
    destination: `s3://${environment}-config/env/${service}/${service}.env`,
    branch: environment,
    commit: 'abc1230000000000000000000000000000000000',
    fallback: false,
    sha256: fakeHash(11),
    size: 812,
    previous_exists: change !== 'new',
    previous_sha256: change === 'new' ? undefined : fakeHash(change === 'changed' ? 12 : 11),
    change,
    keys: null,
    ...extra,
  }
}

function tomlItem(service: string, environment: string, change: ConfigItem['change']): ConfigItem {
  return {
    kind: 'toml',
    name: `${service}.toml`,
    source: `s3://main-config-bucket/${environment}/${service}.toml`,
    destination: `s3://${environment}-config/toml/${service}.toml`,
    source_version_id: 'v-3fa91',
    sha256: fakeHash(21),
    size: 244,
    previous_exists: change !== 'new',
    previous_sha256: change === 'new' ? undefined : fakeHash(change === 'changed' ? 22 : 21),
    change,
  }
}

function planService(run: RunDetail, service: string, version: string, config: ConfigItem[], previousVersion: string): PlanService {
  return {
    name: service,
    group: run.steps[0]?.group ?? '',
    version,
    image_digest: `sha256:${fakeHash(version.length * 31 + service.length)}`,
    ecs_service: `${service}-${run.environment}`,
    ecr_repository: `${run.aws_account}.dkr.ecr.eu-central-1.amazonaws.com/${service}`,
    previous_task_definition: `arn:aws:ecs:eu-central-1:${run.aws_account}:task-definition/${service}-${run.environment}:41`,
    previous_version: previousVersion,
    config,
  }
}

/** A passed set of checks for every service, in the shape the preflight produces. */
export function passedChecks(services: string[]): Check[] {
  return services.flatMap((service) => [
    { name: 'image-exists', service, status: 'passed' as const, detail: `The image for ${service} is in ECR` },
    { name: 'task-definition-exists', service, status: 'passed' as const, detail: 'The previous task definition is active' },
  ])
}

/** A plan for a run built by buildRun: unchanged config unless the caller passes its own. */
export function buildPlan(
  run: RunDetail,
  services: { name: string; version: string; previous: string; config?: ConfigItem[] }[],
  checks?: Check[],
): Plan {
  return {
    id: run.manifest_id,
    run_id: run.id,
    environment: run.environment,
    label: run.label,
    aws_account: run.aws_account,
    inventory_commit: run.inventory_commit,
    job_commit: run.job_commit,
    services: services.map((service) =>
      planService(
        run,
        service.name,
        service.version,
        service.config ?? [envItem(service.name, run.environment, 'unchanged')],
        service.previous,
      ),
    ),
    checks: checks ?? passedChecks(services.map((service) => service.name)),
    ...(run.restore_of ? { restore_of: run.restore_of } : {}),
  }
}

// ---- Log lines ---------------------------------------------------------------------------------------

/** Log lines for a run: one run-level line, then a start line, a result line for every leg that started. */
export function buildLogLines(run: RunDetail): ApiLogLine[] {
  const lines: ApiLogLine[] = [
    { leg_id: 0, seq: 1, at: run.created_at, level: 'info', text: `Run #${run.id} created by ${run.requested_by_name}` },
  ]
  const seqs = new Map<number, number>()
  const next = (legId: number) => {
    const seq = (seqs.get(legId) ?? 0) + 1
    seqs.set(legId, seq)
    return seq
  }
  for (const leg of run.legs) {
    if (!leg.started_at) continue
    const where = leg.service ? ` for ${leg.service}` : ''
    const what = leg.kind === 'step' ? leg.step : leg.kind
    lines.push({ leg_id: leg.id, seq: next(leg.id), at: leg.started_at, level: 'info', text: `${what}${where}: started` })
    if (leg.state === 'succeeded') {
      lines.push({ leg_id: leg.id, seq: next(leg.id), at: leg.finished_at ?? leg.started_at, level: 'ok', text: `${what}${where}: done` })
    } else if (leg.state === 'failed' || leg.state === 'unknown') {
      lines.push({ leg_id: leg.id, seq: next(leg.id), at: leg.finished_at ?? leg.started_at, level: 'error', text: leg.failure_detail || leg.failure_short })
    }
  }
  return lines
}

// ---- The runs ------------------------------------------------------------------------------------------

const dana = { id: 1, name: 'Dana Levi' }
const omer = { id: 3, name: 'Omer Katz' }
const yael = { id: 6, name: 'Yael Barak' }

function runsFixture(): { runs: RunDetail[]; plans: Record<number, Plan> } {
  const runs: RunDetail[] = []
  const plans: Record<number, Plan> = {}
  const add = (run: RunDetail, plan: Plan) => {
    runs.push(run)
    plans[run.id] = plan
  }

  // 101: planned; backend-api's env file changed (and came from the dev branch because the version has none).
  const planned = buildRun({
    id: 101,
    job: 3,
    environment: 'preprod-01',
    state: 'planned',
    services: { 'backend-api': '1.9.4', billing: '2.4.0' },
    legs: standardLegs(['backend-api', 'billing'], {}),
    requestedBy: dana,
    createdAt: '2026-11-27T11:40:00Z',
    waitReason: 'Planned. Nothing runs until someone starts it.',
    selection: { portal: { version: '1.9.4', services: { billing: '2.4.0' } } },
  })
  add(
    planned,
    buildPlan(
      planned,
      [
        {
          name: 'backend-api',
          version: '1.9.4',
          previous: '1.9.3',
          config: [
            envItem('backend-api', 'preprod-01', 'changed', {
              branch: 'dev',
              fallback: true,
              keys: { added: ['FEATURE_X_ENABLED'], removed: ['OLD_TIMEOUT'], changed: ['DB_POOL_SIZE'] },
            }),
          ],
        },
        { name: 'billing', version: '2.4.0', previous: '2.3.9', config: [tomlItem('billing', 'preprod-01', 'unchanged')] },
      ],
      [
        ...passedChecks(['backend-api', 'billing']),
        { name: 'config-source-version', service: 'billing', status: 'not_verified', detail: 'The bucket has no versioning, so the exact file cannot be pinned' },
      ],
    ),
  )

  // 102: planned with a failed check.
  const failedCheck = buildRun({
    id: 102,
    job: 2,
    environment: 'hetzner-01',
    state: 'planned',
    services: { billing: '2.5.0' },
    legs: standardLegs(['billing'], {}),
    requestedBy: omer,
    createdAt: '2026-11-27T11:30:00Z',
    waitReason: 'Planned. Nothing runs until someone starts it.',
    selection: { billing: { version: '2.5.0', services: { billing: '2.5.0' } } },
  })
  add(
    failedCheck,
    buildPlan(
      failedCheck,
      [{ name: 'billing', version: '2.5.0', previous: '2.4.0', config: [tomlItem('billing', 'hetzner-01', 'new')] }],
      [
        { name: 'image-exists', service: 'billing', status: 'failed', detail: 'Image billing:2.5.0 was not found in ECR' },
        { name: 'task-definition-exists', service: 'billing', status: 'passed', detail: 'The previous task definition is active' },
      ],
    ),
  )

  // 103: waiting for a second person.
  const waiting = buildRun({
    id: 103,
    job: 1,
    environment: 'prod-01',
    state: 'waiting_approval',
    services: { 'backend-api': '1.9.4' },
    legs: standardLegs(['backend-api'], {}),
    requestedBy: yael,
    createdAt: '2026-11-27T11:15:00Z',
    startedAt: null,
    waitReason: 'Waiting for someone with approval rights in prod',
    selection: { backends: { version: '1.9.4', services: { 'backend-api': '1.9.4' } } },
    approval: {
      requested_by: yael.id,
      requested_by_name: yael.name,
      requested_at: '2026-11-27T11:20:00Z',
      expires_at: '2026-11-27T15:20:00Z',
      decision: 'pending',
      decided_by_name: null,
      decided_at: null,
      comment: '',
    },
  })
  add(waiting, buildPlan(waiting, [{ name: 'backend-api', version: '1.9.4', previous: '1.9.3', config: [envItem('backend-api', 'prod-01', 'unchanged')] }]))

  // 104: running.
  const running = buildRun({
    id: 104,
    job: 3,
    environment: 'hetzner-01',
    state: 'running',
    services: { 'backend-api': '1.9.4', billing: '2.4.0' },
    legs: standardLegs(
      ['backend-api', 'billing'],
      {
        revalidate: 'succeeded',
        perStep: { 'copy-config': { 'backend-api': 'succeeded', billing: 'running' }, deploy: { 'backend-api': 'running' } },
      },
    ),
    requestedBy: omer,
    createdAt: '2026-11-27T11:48:00Z',
    startedAt: '2026-11-27T11:50:00Z',
    locks: ['hetzner-01/backend-api', 'hetzner-01/billing'],
    autoStart: true,
    selection: { portal: { version: '1.9.4', services: { billing: '2.4.0' } } },
  })
  add(running, buildPlan(running, [{ name: 'backend-api', version: '1.9.4', previous: '1.9.3' }, { name: 'billing', version: '2.4.0', previous: '2.3.9', config: [tomlItem('billing', 'hetzner-01', 'changed')] }]))

  // 105: billing failed; what depended on it was skipped (this run has a third step, verify).
  const partial = buildRun({
    id: 105,
    job: 3,
    environment: 'hetzner-02',
    state: 'partially_succeeded',
    services: { 'backend-api': '1.9.3', billing: '2.3.9' },
    steps: [
      { name: 'copy-config', use: 'copy-config' },
      { name: 'deploy', use: 'deploy-ecs' },
      { name: 'verify', use: 'verify-health' },
    ],
    legs: standardLegs(
      ['backend-api', 'billing'],
      {
        revalidate: 'succeeded',
        perStep: {
          'copy-config': { 'backend-api': 'succeeded', billing: 'failed' },
          deploy: { 'backend-api': 'succeeded', billing: 'skipped' },
          verify: { 'backend-api': 'succeeded', billing: 'skipped' },
        },
      },
      ['copy-config', 'deploy', 'verify'],
    ).map((leg) =>
      leg.step === 'copy-config' && leg.service === 'billing'
        ? { ...leg, short: 'Source file not found', detail: 'copy-config: s3://main-config-bucket/hetzner-02/billing.toml does not exist (NoSuchKey)' }
        : leg,
    ),
    requestedBy: dana,
    createdAt: '2026-11-27T10:00:00Z',
    startedAt: '2026-11-27T10:02:00Z',
    finishedAt: '2026-11-27T10:09:30Z',
    endReason: 'billing failed; its later steps were skipped',
    selection: { portal: { version: '1.9.3', services: { billing: '2.3.9' } } },
  })
  add(partial, buildPlan(partial, [{ name: 'backend-api', version: '1.9.3', previous: '1.9.2' }, { name: 'billing', version: '2.3.9', previous: '2.3.8', config: [tomlItem('billing', 'hetzner-02', 'changed')] }]))

  // 106: the engine lost track of two deploys; the locks stay until a person decides.
  const reconcile = buildRun({
    id: 106,
    job: 3,
    environment: 'preprod-01',
    state: 'needs_reconciliation',
    services: { 'backend-api': '1.9.3', billing: '2.3.9' },
    legs: standardLegs(['backend-api', 'billing'], {
      revalidate: 'succeeded',
      perStep: { 'copy-config': { 'backend-api': 'succeeded', billing: 'succeeded' }, deploy: { 'backend-api': 'unknown', billing: 'unknown' } },
    }).map((leg) =>
      leg.state === 'unknown'
        ? { ...leg, short: 'Lost track of the deployment', detail: `deploy for ${leg.service}: the runner stopped answering after the service update was sent` }
        : leg,
    ),
    requestedBy: dana,
    createdAt: '2026-11-27T09:00:00Z',
    startedAt: '2026-11-27T09:02:00Z',
    waitReason: 'Two steps may or may not have finished. Someone has to check what happened.',
    locks: ['preprod-01/backend-api', 'preprod-01/billing'],
    selection: { portal: { version: '1.9.3', services: { billing: '2.3.9' } } },
  })
  add(reconcile, buildPlan(reconcile, [{ name: 'backend-api', version: '1.9.3', previous: '1.9.2' }, { name: 'billing', version: '2.3.9', previous: '2.3.8' }]))

  // 107: the plan went out of date before it was started.
  const invalidated = buildRun({
    id: 107,
    job: 2,
    environment: 'hetzner-02',
    state: 'invalidated',
    services: { billing: '2.4.0' },
    legs: standardLegs(['billing'], { revalidate: 'failed' }).map((leg) =>
      leg.step === 'revalidate'
        ? { ...leg, short: 'The plan is out of date', detail: 'billing: the task definition changed after the plan was made' }
        : leg.step === 'resolve'
          ? leg
          : { ...leg, state: 'cancelled' as const },
    ),
    requestedBy: omer,
    createdAt: '2026-11-27T08:00:00Z',
    startedAt: '2026-11-27T08:05:00Z',
    finishedAt: '2026-11-27T08:06:00Z',
    endReason: 'billing changed after the plan was made',
    selection: { billing: { version: '2.4.0', services: { billing: '2.4.0' } } },
  })
  add(invalidated, buildPlan(invalidated, [{ name: 'billing', version: '2.4.0', previous: '2.3.9' }]))

  // 109: a plain success, and 108: the restore of it.
  const succeeded = buildRun({
    id: 109,
    job: 1,
    environment: 'hetzner-01',
    state: 'succeeded',
    services: { 'backend-api': '1.9.4' },
    legs: standardLegs(['backend-api'], { revalidate: 'succeeded', perStep: { 'copy-config': { 'backend-api': 'succeeded' }, deploy: { 'backend-api': 'succeeded' } } }),
    requestedBy: dana,
    createdAt: '2026-11-26T15:00:00Z',
    startedAt: '2026-11-26T15:01:00Z',
    finishedAt: '2026-11-26T15:06:00Z',
    selection: { backends: { version: '1.9.4', services: { 'backend-api': '1.9.4' } } },
  })
  const restore = buildRun({
    id: 108,
    job: 1,
    environment: 'hetzner-01',
    state: 'succeeded',
    kind: 'restore',
    restoreOf: 109,
    services: { 'backend-api': '1.9.3' },
    steps: [
      { name: 'restore-config', use: 'copy-config' },
      { name: 'restore-deploy', use: 'ecs-deploy' },
    ],
    legs: standardLegs(
      ['backend-api'],
      { revalidate: 'succeeded', perStep: { 'restore-config': { 'backend-api': 'succeeded' }, 'restore-deploy': { 'backend-api': 'succeeded' } } },
      ['restore-config', 'restore-deploy'],
    ),
    requestedBy: dana,
    createdAt: '2026-11-27T07:00:00Z',
    startedAt: '2026-11-27T07:02:00Z',
    finishedAt: '2026-11-27T07:05:00Z',
  })
  const restorePlan = buildPlan(restore, [
    {
      name: 'backend-api',
      version: '1.9.3',
      previous: '1.9.4',
      config: [{ ...envItem('backend-api', 'hetzner-01', 'changed', { not_restored: '' }) }],
    },
  ])
  const restoreService = restorePlan.services[0]
  if (restoreService) {
    restoreService.target_task_definition = `arn:aws:ecs:eu-central-1:111111111111:task-definition/backend-api-hetzner-01:40`
    restoreService.config.push({ ...tomlItem('backend-api', 'hetzner-01', 'new'), not_restored: 'did not exist before run #109; left in place' })
  }
  add(succeeded, buildPlan(succeeded, [{ name: 'backend-api', version: '1.9.4', previous: '1.9.3' }]))
  add(restore, restorePlan)

  runs.sort((a, b) => a.id - b.id)
  return { runs, plans }
}

const built = runsFixture()
export const runsFixtureList: RunDetail[] = built.runs
export const plansFixture: Record<number, Plan> = built.plans

/** Log lines of every fixture run that has started, by run id. */
export const logsFixture: Record<number, ApiLogLine[]> = Object.fromEntries(
  built.runs.map((run) => [run.id, buildLogLines(run)]),
)
