// A complete in-memory stand-in for the Yumdrake API, for tests. It answers every route of the contract,
// enforces permissions the way the server does (401 when signed out, 403 when the grant is missing) and
// keeps state that changes as tests create, update and delete things. Everything resets before each test.
//
// Helpers for tests (all re-exported from ./server):
//   signInAs(me) / signOut()            who the mock server believes is signed in
//   failNext(method, path, status, code, message?, headers?)   the next matching request fails once
//   delayNext(method, path, ms)         the next matching request answers after ms
//   requests                            every request the server saw, in order
//   mockState()                         the live state, for arranging or checking things
//   resetMockApi()                      done for you in setup.ts after each test
import { http, HttpResponse } from 'msw'
import type { HttpHandler } from 'msw'
import { can } from '../auth/permissions'
import type { Target } from '../auth/permissions'
import { ALL_ACTIONS } from '../api/types'
import type {
  Action,
  AdminGroup,
  AdminUser,
  ApiLogLine,
  Approval,
  ApprovalItem,
  AuditEntry,
  AwsAccount,
  AwsCredentialInput,
  AwsCredentialView,
  CreatedToken,
  ErrorBody,
  Freeze,
  GithubConnection,
  ImageVersion,
  Inventory,
  JobDetail,
  JobSummary,
  JobVersions,
  Me,
  Overview,
  QueueView,
  RunningCell,
  RunningMatrix,
  ServicePage,
  SystemHealth,
  Problem,
  RunDetail,
  JobGroup,
  RunLeg,
  RunState,
  Selection,
} from '../api/types'
import { buildJobDefinition, createMockState, knownPassword, mePresets } from './fixtures'
import type { MockState, StoredJob } from './fixtures'
import { buildPlan, buildRun, fakeHash, runRow, standardLegs, summarizeLegs } from './runFixtures'
import type { RunJob } from './runFixtures'

// ---- State, request log, injected failures and delays ------------------------------

let state: MockState = createMockState()

export interface SeenRequest {
  method: string
  /** The concrete path, for example "/api/v1/jobs/3". */
  path: string
  /** The query string including "?", or "". */
  search: string
  /** The parsed JSON body for requests that had one, otherwise undefined. */
  body: unknown
  /** The status the mock server answered with; undefined while it is still working on it. */
  status: number | undefined
}

/** Every request the mock server has seen since the last reset. */
export const requests: SeenRequest[] = []

interface InjectedFailure {
  status: number
  code: string
  message: string
  headers: Record<string, string>
}

const failures = new Map<string, InjectedFailure[]>()
const delays = new Map<string, number[]>()

const keyOf = (method: string, path: string) => `${method.toUpperCase()} ${path}`

/** The state behind the mock API; change it to arrange a scenario, read it to check a result. */
export function mockState(): MockState {
  return state
}

/** Puts everything back to the fixtures, signed in as the admin. Called after every test by setup.ts. */
export function resetMockApi(): void {
  state = createMockState()
  requests.length = 0
  failures.clear()
  delays.clear()
}

/** From now on the mock server treats this person as signed in (the same as having a valid session cookie). */
export function signInAs(me: Me): void {
  state.session = structuredClone(me)
}

/** From now on every route that needs a session answers 401. */
export function signOut(): void {
  state.session = null
}

/**
 * The next request with this method and path answers with this error, once, before any permission check.
 * `path` is the concrete path without the query, for example "/api/v1/jobs/3".
 */
export function failNext(
  method: string,
  path: string,
  status: number,
  code: string,
  message = 'Simulated failure',
  headers: Record<string, string> = {},
): void {
  const key = keyOf(method, path)
  failures.set(key, [...(failures.get(key) ?? []), { status, code, message, headers }])
}

/** The next request with this method and path is answered after `ms` milliseconds (real or fake timers). */
export function delayNext(method: string, path: string, ms: number): void {
  const key = keyOf(method, path)
  delays.set(key, [...(delays.get(key) ?? []), ms])
}

// ---- Small helpers -------------------------------------------------------------------

const json = (body: unknown, status = 200, headers: Record<string, string> = {}) =>
  HttpResponse.json(body as Record<string, unknown>, { status, headers })

const noContent = () => new HttpResponse(null, { status: 204 })

function errorResponse(status: number, code: string, message: string, headers: Record<string, string> = {}) {
  const body: ErrorBody = { error: { code, message } }
  return HttpResponse.json(body, { status, headers })
}

const invalid = (message: string) => errorResponse(400, 'invalid', message)
const notFound = (what: string) => errorResponse(404, 'not_found', `${what} not found`)
const conflict = (message: string) => errorResponse(409, 'conflict', message)

type Fields = Record<string, unknown>

function fields(body: unknown): Fields {
  return typeof body === 'object' && body !== null ? (body as Fields) : {}
}
const text = (value: unknown): string => (typeof value === 'string' ? value : '')
const whole = (value: unknown): number => (typeof value === 'number' && Number.isFinite(value) ? value : 0)

function takeId(prefix: string): number {
  const id = state.nextId[prefix] ?? 1
  state.nextId[prefix] = id + 1
  return id
}

const now = () => '2026-11-27T12:00:00Z'

function recordAudit(action: string, targetType: string, targetId: string | number, detail: Record<string, unknown> = {}) {
  const entry: AuditEntry = {
    id: takeId('audit'),
    at: now(),
    actor: state.session?.email ?? 'system',
    actor_id: state.session?.id ?? null,
    action,
    target_type: targetType,
    target_id: String(targetId),
    detail,
  }
  state.audit.unshift(entry)
}

// ---- Routes ---------------------------------------------------------------------------

type Guard = 'public' | 'session' | { action: Action; target?: (ctx: Context) => Target }

interface Params {
  id: string
  name: string
  idpGroup: string
  leg: string
}

interface Context {
  me: Me | null
  params: Params
  url: URL
  body: unknown
}

type Resolver = (ctx: Context) => Response | Promise<Response>

const factories = { GET: http.get, POST: http.post, PUT: http.put, PATCH: http.patch, DELETE: http.delete }

function route(method: keyof typeof factories, path: string, guard: Guard, resolver: Resolver): HttpHandler {
  return factories[method](path, async ({ request, params }) => {
    const url = new URL(request.url)
    let body: unknown
    if (method !== 'GET') {
      const raw = await request.text()
      try {
        body = raw === '' ? undefined : JSON.parse(raw)
      } catch {
        body = raw
      }
    }
    const seen: SeenRequest = { method, path: url.pathname, search: url.search, body, status: undefined }
    requests.push(seen)
    const finish = (response: Response) => {
      seen.status = response.status
      return response
    }

    const key = keyOf(method, url.pathname)
    const waits = delays.get(key)
    const wait = waits?.shift()
    if (wait !== undefined) {
      await new Promise((resolve) => setTimeout(resolve, wait))
    }
    const failure = failures.get(key)?.shift()
    if (failure) {
      return finish(errorResponse(failure.status, failure.code, failure.message, failure.headers))
    }

    const me = state.session
    const context: Context = {
      me,
      params: { id: String(params.id ?? ''), name: String(params.name ?? ''), idpGroup: String(params.idpGroup ?? ''), leg: String(params.leg ?? '') },
      url,
      body,
    }
    if (guard !== 'public') {
      if (me === null) {
        return finish(errorResponse(401, 'unauthorized', 'You are not signed in'))
      }
      if (guard !== 'session') {
        const target = guard.target ? guard.target(context) : {}
        if (!can(me, guard.action, target)) {
          return finish(errorResponse(403, 'forbidden', 'You do not have permission to do this'))
        }
      }
    }
    return finish(await resolver(context))
  })
}

const view = { action: 'view' } as const
const manageJobs = (target?: (ctx: Context) => Target): Guard => ({ action: 'manage_jobs', target })
const manageConnections: Guard = { action: 'manage_connections' }
const manageAccess: Guard = { action: 'manage_access' }

// ---- Derived views ----------------------------------------------------------------------

function jobCounts(job: StoredJob): Pick<JobSummary, 'services' | 'approval_labels'> {
  const services = new Set((job.definition?.groups ?? []).flatMap((group) => group.services ?? []))
  return { services: services.size, approval_labels: job.definition?.approval?.labels ?? [] }
}

function jobSummary(job: StoredJob): JobSummary {
  const { definition: _definition, source: _source, problems: _problems, problems_commit: _problemsCommit, ...summary } = job
  void _definition
  void _source
  void _problems
  void _problemsCommit
  return { ...summary, ...jobCounts(job) }
}

function jobDetail(job: StoredJob): JobDetail {
  return { ...job, ...jobCounts(job) }
}

function findJob(idText: string): StoredJob | undefined {
  return state.jobs.find((job) => String(job.id) === idText)
}

function userView(user: AdminUser): AdminUser {
  return user
}

function groupView(group: AdminGroup): AdminGroup {
  return { ...group, member_count: state.users.filter((user) => user.groups.some((ref) => ref.id === group.id)).length }
}

/** At least one enabled user in an admin group must remain (Plan 3, Task 6). */
function hasAdministratorLeft(): boolean {
  const adminGroupIds = new Set(state.groups.filter((group) => group.admin).map((group) => group.id))
  return state.users.some((user) => !user.disabled && user.groups.some((ref) => adminGroupIds.has(ref.id)))
}

function awsView(input: AwsCredentialInput): AwsCredentialView {
  return {
    method: input.method,
    role_arn: input.method === 'role' ? (input.role_arn ?? '') : '',
    external_id: input.method === 'role' ? (input.external_id ?? '') : '',
    access_key_id_last4: input.method === 'key' ? (input.access_key_id ?? '').slice(-4) : '',
  }
}

function awsCredentialProblem(label: string, input: unknown): string {
  const credential = fields(input)
  if (credential.method === 'role') {
    return text(credential.role_arn) === '' ? `${label} needs a role ARN` : ''
  }
  if (credential.method === 'key') {
    return text(credential.access_key_id) === '' || text(credential.secret_access_key) === ''
      ? `${label} needs an access key id and a secret access key`
      : ''
  }
  return `${label} method must be "role" or "key"`
}

function syncInventoryNow(): Inventory {
  state.inventory.checked_at = now()
  return state.inventory
}

const ownerPattern = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$/
const validRepo = (repo: string) => /^[^\s/]+\/[^\s/]+$/.test(repo)

// ---- Runs, approvals, freezes and Slack (helpers) -------------------------------------------------

const forbiddenResponse = () => errorResponse(403, 'forbidden', 'You do not have permission to do this')

const runStateNames: RunState[] = [
  'resolving',
  'planned',
  'waiting_approval',
  'queued',
  'running',
  'cancelling',
  'needs_reconciliation',
  'succeeded',
  'partially_succeeded',
  'failed',
  'cancelled',
  'rejected',
  'expired',
  'invalidated',
]

const runTarget = (run: RunDetail): Target => ({ label: run.label, environment: run.environment, folderId: run.folder_id })

function findRun(idText: string): RunDetail | undefined {
  return state.runs.find((run) => String(run.id) === idText)
}

/** The run as the server sends it. Whether this person may decide the approval is in GET /approvals, not here. */
function runView(run: RunDetail): RunDetail {
  const view = structuredClone(run)
  // Go sends null for a run that holds no locks (the list is a nil slice there); the client turns it into [].
  if (view.locks.length === 0) {
    ;(view as unknown as { locks: null }).locks = null
  }
  return view
}

/** Who may approve: the grant in the run's label, and not the person who started it (an admin may). */
function approvalAbility(me: Me, run: RunDetail): { can: boolean; reason: string } {
  if (!can(me, 'approve', runTarget(run))) {
    return { can: false, reason: `You do not have permission to approve in ${run.label}.` }
  }
  if (run.requested_by === me.id && !me.admin) {
    return { can: false, reason: 'You started this run, so someone else has to approve it.' }
  }
  return { can: true, reason: '' }
}

function wrongState(action: string, run: RunDetail, extra = ''): Response {
  return conflict(`${action} run ${run.id}: run is not in a state that allows this: it is ${run.state}${extra}`)
}

function appendRunLine(run: RunDetail, level: ApiLogLine['level'], text: string): void {
  const lines = (state.logs[run.id] ??= [])
  const seq = lines.filter((line) => line.leg_id === 0).length + 1
  lines.push({ leg_id: 0, seq, at: now(), level, text })
}

const finalStates: RunState[] = ['succeeded', 'partially_succeeded', 'failed', 'cancelled', 'rejected', 'expired', 'invalidated']

function moveRun(run: RunDetail, next: RunState, reason: string): void {
  run.state = next
  run.state_changed_at = now()
  if (finalStates.includes(next)) {
    run.wait_reason = ''
    run.end_reason = reason
    run.finished_at = now()
  } else {
    run.wait_reason = reason
    run.end_reason = ''
  }
}

function refreshSummary(run: RunDetail): void {
  run.summary = summarizeLegs(run.legs)
}

function runJobInfo(job: StoredJob): RunJob {
  return {
    sourceId: job.id,
    jobId: job.job_id,
    title: job.title,
    folderId: job.folder_id,
    groups: (job.definition?.groups ?? []).map((group) => ({ id: group.id, services: group.services ?? [] })),
    approvalLabels: job.definition?.approval?.labels ?? [],
  }
}

function approvalFor(requestedBy: Me, expiresInMs: number): Approval {
  return {
    requested_by: requestedBy.id,
    requested_by_name: requestedBy.name,
    requested_at: now(),
    expires_at: new Date(Date.parse(now()) + expiresInMs).toISOString(),
    decision: 'pending',
    decided_by_name: null,
    decided_at: null,
    comment: '',
  }
}

/** Moves a planned run on the way Start does: to approval when the job needs it, otherwise to the queue. */
function startRun(run: RunDetail, me: Me): void {
  if (run.approval_required) {
    const job = state.jobs.find((candidate) => candidate.id === run.job_source_id)
    const expires = job?.definition?.approval?.expires ? job.definition.approval.expires / 1e6 : 4 * 3600 * 1000
    run.approval = approvalFor(me, expires)
    moveRun(run, 'waiting_approval', 'Waiting for someone with approval rights in ' + run.label)
  } else {
    moveRun(run, 'queued', 'Waiting for its turn')
  }
  appendRunLine(run, 'info', `Started by ${me.name}`)
}

interface NewRunInput {
  job: StoredJob
  environment: string
  services: Record<string, string>
  stepNames?: { name: string; use: string }[]
  kind?: 'deploy' | 'restore'
  restoreOf?: number
  selection?: Selection
  me: Me
}

/** Creates a planned run (the mock skips "resolving": the plan is ready at once) and stores its plan and log. */
function createPlannedRun({ job, environment, services, stepNames, kind, restoreOf, selection, me }: NewRunInput): RunDetail {
  const id = takeId('run')
  const names = (stepNames ?? job.definition?.steps?.map((step) => ({ name: step.name, use: step.use })) ?? [{ name: 'deploy', use: 'deploy-ecs' }]).map(
    (step) => step.name,
  )
  const run = buildRun({
    id,
    job: job.id,
    jobInfo: runJobInfo(job),
    environment,
    state: 'planned',
    kind,
    restoreOf,
    services,
    steps: stepNames ?? job.definition?.steps?.map((step) => ({ name: step.name, use: step.use })),
    legs: standardLegs(Object.keys(services), {}, names),
    requestedBy: { id: me.id, name: me.name },
    createdAt: now(),
    waitReason: 'Planned. Nothing runs until someone starts it.',
    selection,
  })
  state.runs.push(run)
  state.plans[id] = buildPlan(
    run,
    Object.entries(services).map(([name, version]) => ({ name, version, previous: '' })),
  )
  state.logs[id] = []
  appendRunLine(run, 'info', `Run #${id} created by ${me.name}`)
  return run
}

/** The services of the group that its selection entry names. */
function namedServices(group: JobGroup, selection: Fields): string[] {
  if (!(group.id in selection)) return []
  const named = fields(fields(selection[group.id]).services)
  return (group.services ?? []).filter((service) => service in named)
}

function selectionProblems(job: StoredJob, environment: string, selection: Fields): Problem[] {
  const problems: Problem[] = []
  const known = state.inventory.environments.some((candidate) => candidate.name === environment)
  if (!known) {
    problems.push({ file: job.path, line: 0, message: `environment "${environment}" is not in the inventory` })
  }
  const groups = job.definition?.groups ?? []
  // As in Go, only the services a group names are selected: a group missing from the selection, or naming no
  // service, selects nothing, and a selection that selects no service is refused.
  const chosen = groups.filter((group) => namedServices(group, selection).length > 0)
  if (chosen.length === 0) {
    problems.push({ file: job.path, line: 0, message: 'nothing is selected' })
  }
  for (const group of chosen) {
    const choice = fields(selection[group.id])
    if (text(choice.version).trim() === '') {
      problems.push({ file: job.path, line: group.line, message: `group "${group.id}" needs a version` })
      continue
    }
    for (const service of namedServices(group, selection)) {
      const summary = state.inventory.services.find((candidate) => candidate.name === service)
      if (known && !summary?.environments.includes(environment)) {
        problems.push({ file: job.path, line: group.line, message: `service "${service}" has no block for environment "${environment}"` })
      }
    }
  }
  for (const name of Object.keys(selection)) {
    if (!groups.some((group) => group.id === name)) {
      problems.push({ file: job.path, line: 0, message: `the job has no group "${name}"` })
    }
  }
  return problems
}

/** service -> version for a selection that passed selectionProblems. */
function servicesOfSelection(job: StoredJob, selection: Fields): Record<string, string> {
  const result: Record<string, string> = {}
  for (const group of job.definition?.groups ?? []) {
    const choice = fields(selection[group.id])
    const overrides = fields(choice.services)
    for (const service of namedServices(group, selection)) {
      result[service] = text(overrides[service]) || text(choice.version)
    }
  }
  return result
}

/** The selection that would plan these services again: one entry per group, overrides where versions differ. */
function selectionOfServices(job: StoredJob, services: Record<string, string>): Selection {
  const selection: Selection = {}
  for (const group of job.definition?.groups ?? []) {
    const mine = (group.services ?? []).filter((service) => service in services)
    const first = mine[0]
    if (first === undefined) continue
    const base = services[first] ?? ''
        selection[group.id] = { version: base, services: Object.fromEntries(mine.map((service) => [service, services[service] ?? base])) }
  }
  return selection
}

function serviceVersionsOf(run: RunDetail): Record<string, string> {
  return Object.fromEntries(run.legs.filter((leg) => leg.kind === 'step').map((leg) => [leg.service, leg.version]))
}

/** Services whose step legs ended in a way that changed something. */
function changedServices(run: RunDetail): string[] {
  return [...new Set(run.legs.filter((leg) => leg.kind === 'step' && leg.state === 'succeeded').map((leg) => leg.service))]
}

function unknownLeg(run: RunDetail, legIdText: string): RunLeg | undefined {
  return run.legs.find((leg) => String(leg.id) === legIdText)
}

/** What is left to say about a run once every unknown leg has been decided. */
function settleReconciledRun(run: RunDetail): void {
  if (run.legs.some((leg) => leg.state === 'unknown')) {
    return
  }
  for (const leg of run.legs) {
    if (leg.state === 'pending' || leg.state === 'queued') {
      leg.state = 'skipped'
    }
  }
  const failed = run.legs.some((leg) => leg.state === 'failed')
  const succeeded = run.legs.some((leg) => leg.kind === 'step' && leg.state === 'succeeded')
  if (!failed) {
    moveRun(run, 'succeeded', '')
  } else {
    moveRun(run, succeeded ? 'partially_succeeded' : 'failed', 'Decided by hand')
  }
  run.locks = []
}

/** A run-scoped route: 404 for an unknown run first, then 403 (like the server), then the resolver. */
function runRoute(
  method: keyof typeof factories,
  path: string,
  action: Action,
  resolver: (ctx: Context, run: RunDetail, me: Me) => Response | Promise<Response>,
): HttpHandler {
  return route(method, path, 'session', (ctx) => {
    const run = findRun(ctx.params.id)
    if (!run) {
      return notFound('run')
    }
    const me = ctx.me as Me
    if (!can(me, action, runTarget(run))) {
      return forbiddenResponse()
    }
    return resolver(ctx, run, me)
  })
}

function imageVersions(service: string): ImageVersion[] {
  const tags: Record<string, string[]> = { 'backend-api': ['1.9.4', '1.9.3', '1.9.2', '1.9.1'], billing: ['2.4.0', '2.3.9', '2.3.8'] }
  return (tags[service] ?? ['1.0.0']).map((tag, index) => ({
    tag,
    digest: `sha256:${fakeHash(tag.length * 7 + index + service.length)}`,
    pushed_at: new Date(Date.parse(now()) - (index + 1) * 86_400_000).toISOString(),
  }))
}

function versionsOfJob(job: StoredJob, environment: string): JobVersions {
  const environmentServices = new Set(
    state.inventory.services.filter((service) => service.environments.includes(environment)).map((service) => service.name),
  )
  const groups: JobVersions['groups'] = {}
  for (const group of job.definition?.groups ?? []) {
    const names = group.services ?? []
    const present = names.filter((name) => environmentServices.has(name))
    const lists = Object.fromEntries(present.map((name) => [name, imageVersions(name)]))
    const first = present[0]
    const common = first === undefined ? [] : (lists[first] ?? []).map((v) => v.tag).filter((tag) => present.every((name) => (lists[name] ?? []).some((v) => v.tag === tag)))
    groups[group.id] = {
      common,
      services: lists,
      unavailable: Object.fromEntries(names.filter((name) => !environmentServices.has(name)).map((name) => [name, 'not in this environment'])),
    }
  }
  return { groups }
}

function isActiveFreeze(freeze: Freeze): boolean {
  return freeze.ended_at === null && Date.parse(freeze.expires_at) > Date.parse(now())
}

// ---- Overview fixture (Plan 9) ---------------------------------------------------------------

/** A period of `days` days ending 2026-11-28 with a fixed shape; exported so tests can read the numbers. */
export function overviewFixture(days: number, label = ''): Overview {
  const end = Date.UTC(2026, 10, 28)
  const list = Array.from({ length: days }, (_, index) => {
    const date = new Date(end - (days - 1 - index) * 86_400_000).toISOString().slice(0, 10)
    return { date, succeeded: index % 3 === 0 ? 0 : 2, failed: index % 5 === 0 ? 1 : 0, other: index === 1 ? 1 : 0 }
  })
  const total = list.reduce((sum, day) => sum + day.succeeded + day.failed + day.other, 0)
  return {
    from: list[0]?.date ?? '',
    to: list[list.length - 1]?.date ?? '',
    runs: total,
    prev_runs: total - 3,
    success_rate: 0.8,
    prev_success_rate: 0.85,
    median_duration_seconds: 134,
    prev_median_duration_seconds: 150,
    waiting_approval: 1,
    oldest_waiting: '2026-11-28T10:00:00Z',
    days: list,
    top_failing: [{ service: 'billing-api', failures: 3, common_reason: 'Health check failed' }],
    attention: [
      {
        run_id: 41,
        state: 'failed',
        job: 'deploy-billing',
        environment: label === '' ? 'hetzner-01' : `${label}-01`,
        label: label === '' ? 'dev' : label,
        reason: 'billing-api did not become healthy',
        by: 'Ada',
        at: '2026-11-28T09:00:00Z',
      },
    ],
    environments: [
      { environment: 'hetzner-01', label: 'dev', run_id: 41, state: 'failed', job: 'deploy-billing', at: '2026-11-28T09:00:00Z' },
      { environment: 'preprod-01', label: 'preprod', run_id: null, state: 'failed', job: '', at: null },
    ],
  }
}

/** A cell that is running `version`, deployed by run #12 unless said otherwise. */
export function runningCell(version: string, over: Partial<RunningCell> = {}): RunningCell {
  return {
    state: 'ok',
    version,
    task_definition: `billing-api:${version.replace(/\D/g, '') || '1'}`,
    by_yumdrake: true,
    run_id: 12,
    last_deploy_failed: false,
    behind: false,
    error: '',
    read_at: '2026-11-28T11:58:00Z',
    ...over,
  }
}

export const absentCell: RunningCell = {
  state: 'absent',
  version: '',
  task_definition: '',
  by_yumdrake: false,
  run_id: 0,
  last_deploy_failed: false,
  behind: false,
  error: '',
  read_at: '',
}

/** The default answer of GET /api/v1/versions/running: two environments of different labels. */
export function runningFixture(): RunningMatrix {
  return {
    environments: [
      { name: 'hetzner-01', label: 'dev', aws_account: '111111111111' },
      { name: 'prod-01', label: 'prod', aws_account: '333333333333' },
    ],
    rows: [
      { service: 'backend-api', cells: [runningCell('1.4.2'), runningCell('1.4.2')] },
      { service: 'billing', cells: [runningCell('2.0.1'), absentCell] },
    ],
  }
}

/** The default answer of GET /api/v1/queue: two runners, one run running, one waiting on it, one lock. */
export function queueFixture(): QueueView {
  return {
    runs: [
      {
        id: 41,
        job_id: 'deploy-billing',
        job_name: 'Deploy billing',
        environment: 'hetzner-01',
        label: 'dev',
        state: 'running',
        wait_reason: '',
        blocked_by_run_id: null,
        waiting_since: '2026-11-28T11:50:00Z',
        waited_seconds: 0,
        legs_running: 1,
        legs_queued: 0,
      },
      {
        id: 42,
        job_id: 'deploy-backend',
        job_name: 'Deploy backend',
        environment: 'hetzner-01',
        label: 'dev',
        state: 'queued',
        wait_reason: 'waiting for run #41',
        blocked_by_run_id: 41,
        waiting_since: '2026-11-28T11:58:00Z',
        waited_seconds: 134,
        legs_running: 0,
        legs_queued: 2,
      },
    ],
    locks: [{ resource: 'ecs:111111111111:eu-west-1:cluster/main:service/billing-api', run_id: 41, acquired_at: '2026-11-28T11:50:00Z' }],
    runners: [
      { id: 'runner-a', version: '0.9.0', concurrency: 8, busy: 3, started_at: '2026-11-28T08:00:00Z', last_seen_at: '2026-11-28T11:59:55Z' },
      { id: 'runner-b', version: '0.9.0', concurrency: 8, busy: 2, started_at: '2026-11-28T08:00:00Z', last_seen_at: '2026-11-28T11:59:58Z' },
    ],
    free_slots: 11,
    queued_legs: 2,
    oldest_queued_at: '2026-11-28T11:58:00Z',
    runner_gone_after_seconds: 120,
  }
}

/** The default answer of GET /api/v1/system/health: everything works. */
export function healthFixture(): SystemHealth {
  const queue = queueFixture()
  return {
    problems: [],
    runners: queue.runners,
    runner_gone_after_seconds: 120,
    queue: { queued_legs: 2, oldest_queued_at: queue.oldest_queued_at, active_runs: 2 },
    counters: {
      lease_expired: { hour: 0, day: 1 },
      log_persist_failed: { hour: 0, day: 0 },
      sync_failed: { hour: 0, day: 2 },
    },
    sync: {
      inventory: { checked_at: '2026-11-28T11:57:00Z', error: '', error_at: null },
      jobs: [{ id: 1, job_id: 'deploy-billing', title: 'Deploy billing', checked_at: '2026-11-28T11:55:00Z', error: '', error_at: null }],
    },
    connections: [
      { kind: 'aws', name: 'homelab', last_ok_at: '2026-11-28T11:59:00Z', last_error_at: null, last_error_code: '', failing: false },
      { kind: 'github', name: 'rcss-labs', last_ok_at: '2026-11-28T11:56:00Z', last_error_at: null, last_error_code: '', failing: false },
    ],
    notifications: { pending: 0, failed_last_day: 0, oldest_pending_at: null },
  }
}

/** The default answer of GET /api/v1/services/{name}. */
export function servicePageFixture(service: ServicePage['service']): ServicePage {
  return {
    service,
    environments: [
      { environment: 'hetzner-01', label: 'dev', cell: runningCell('1.4.2'), restore_from_run_id: 12, restore_from_folder_id: 1 },
      { environment: 'prod-01', label: 'prod', cell: runningCell('1.4.1', { by_yumdrake: false, run_id: 0 }), restore_from_run_id: null, restore_from_folder_id: null },
    ],
    deployments: [
      { run_id: 12, leg_id: 301, environment: 'hetzner-01', version: '1.4.2', state: 'succeeded', at: '2026-11-28T11:00:00Z', by: 'Ada' },
    ],
    config: [
      {
        run_id: 12,
        environment: 'hetzner-01',
        at: '2026-11-28T11:00:00Z',
        items: [{ kind: 'env_file', name: 'app.env', destination: '/etc/app/app.env', checksum: 'a1b2c3d4e5f6' }],
      },
    ],
  }
}

// ---- The handlers --------------------------------------------------------------------------

export const handlers: HttpHandler[] = [
  // Sign-in
  route('GET', '/api/v1/auth/methods', 'public', () => json({ password: true, saml: state.sso.enabled })),

  route('POST', '/api/v1/auth/login', 'public', ({ body }) => {
    const { email, password } = fields(body)
    const person = mePresets.find((preset) => preset.email === email)
    const disabled = state.users.some((user) => user.email === email && user.disabled)
    if (!person || password !== knownPassword || disabled) {
      return errorResponse(401, 'unauthorized', 'email or password is not correct')
    }
    state.session = structuredClone(person)
    return json(person)
  }),

  // Like the server, a sign-out needs a session; the web treats the 401 for none as signed out.
  route('POST', '/api/v1/auth/logout', 'session', () => {
    state.session = null
    return noContent()
  }),

  route('GET', '/api/v1/me', 'session', ({ me }) => json(me)),

  // Inventory
  route('GET', '/api/v1/inventory', view, () => json(state.inventory)),

  route('GET', '/api/v1/inventory/services/:name', view, ({ params }) => {
    const service = state.services.find((candidate) => candidate.name === params.name)
    return service ? json(service) : notFound('service')
  }),

  route('POST', '/api/v1/inventory/sync', manageConnections, () => json(syncInventoryNow())),

  route('GET', '/api/v1/admin/inventory-source', manageConnections, () => json(state.inventorySource)),

  route('PUT', '/api/v1/admin/inventory-source', manageConnections, ({ body }) => {
    const { repo, branch, path } = fields(body)
    if (!validRepo(text(repo))) {
      return invalid(`repo "${text(repo)}" must look like owner/name`)
    }
    if (text(branch).trim() === '') {
      return invalid('branch is required')
    }
    state.inventorySource = { repo: text(repo), branch: text(branch), path: text(path) }
    recordAudit('inventory.source', 'inventory', '', { repo: text(repo), branch: text(branch), path: text(path) })
    return json(syncInventoryNow())
  }),

  // GitHub connections
  route('GET', '/api/v1/admin/github-connections', manageConnections, () =>
    json({ connections: state.githubConnections }),
  ),

  route('POST', '/api/v1/admin/github-connections', manageConnections, ({ body }) => {
    const request = fields(body)
    const owner = text(request.owner).trim().toLowerCase()
    if (!ownerPattern.test(owner)) {
      return invalid(`owner "${text(request.owner)}" is not a valid GitHub user or organisation name`)
    }
    const method = text(request.method)
    const appId = whole(request.app_id)
    const installationId = whole(request.installation_id)
    if (method === 'token') {
      if (text(request.token) === '') {
        return invalid('method "token" requires token')
      }
      if (appId !== 0 || installationId !== 0 || text(request.private_key) !== '') {
        return invalid('method "token" takes only token')
      }
    } else if (method === 'app') {
      if (appId <= 0 || installationId <= 0 || text(request.private_key) === '') {
        return invalid('method "app" requires app_id, installation_id and private_key')
      }
      if (text(request.token) !== '') {
        return invalid('method "app" takes no token')
      }
      if (!text(request.private_key).includes('BEGIN')) {
        return invalid('private_key is not a valid RSA private key in PEM form')
      }
    } else {
      return invalid('method must be "token" or "app"')
    }
    if (state.githubConnections.some((connection) => connection.owner === owner)) {
      return conflict(`owner "${owner}" already has a connection`)
    }
    const created: GithubConnection = {
      id: takeId('connection'),
      owner,
      method,
      app_id: appId,
      installation_id: installationId,
      created_at: now(),
    }
    state.githubConnections.push(created)
    recordAudit('connection.create', 'github_connection', created.id, { owner, method })
    return json(created, 201)
  }),

  // AWS accounts
  route('GET', '/api/v1/admin/aws-accounts', manageConnections, () => json({ accounts: state.awsAccounts })),

  route('POST', '/api/v1/admin/aws-accounts', manageConnections, ({ body }) => {
    const request = fields(body)
    const name = text(request.name).trim()
    const accountId = text(request.account_id).trim()
    if (name === '') {
      return invalid('name is required')
    }
    if (!/^\d{12}$/.test(accountId)) {
      return invalid('account_id must be 12 digits')
    }
    const problem = awsCredentialProblem('discovery', request.discovery) || awsCredentialProblem('deploy', request.deploy)
    if (problem !== '') {
      return invalid(problem)
    }
    if (state.awsAccounts.some((account) => account.name === name || account.account_id === accountId)) {
      return conflict(`an account named "${name}" or with id ${accountId} already exists`)
    }
    const account: AwsAccount = {
      id: takeId('aws'),
      name,
      account_id: accountId,
      discovery: awsView(request.discovery as AwsCredentialInput),
      deploy: awsView(request.deploy as AwsCredentialInput),
      environments: state.inventory.environments.filter((env) => env.aws_account === accountId).map((env) => env.name),
      created_at: now(),
    }
    state.awsAccounts.push(account)
    recordAudit('aws_account.create', 'aws_account', account.id, { name, account_id: accountId })
    return json(account, 201)
  }),

  // An account id ending in 999 answers like a role in another account; every other account is fine.
  route('POST', '/api/v1/admin/aws-accounts/:id/test', manageConnections, ({ params }) => {
    const account = state.awsAccounts.find((candidate) => String(candidate.id) === params.id)
    if (!account) {
      return notFound('AWS account')
    }
    if (account.account_id.endsWith('999')) {
      return json({
        ok: false,
        error_code: 'AccountMismatch',
        message: `The credential belongs to another account than ${account.account_id}`,
      })
    }
    return json({ ok: true, error_code: '', message: `Connected to account ${account.account_id}` })
  }),

  // Folders
  route('GET', '/api/v1/folders', view, () => json({ folders: state.folders })),

  route(
    'POST',
    '/api/v1/folders',
    manageJobs(),
    ({ body }) => {
      const name = text(fields(body).name).trim()
      if (name === '' || name.length > 100) {
        return invalid('name is required and at most 100 characters')
      }
      if (state.folders.some((folder) => folder.name === name)) {
        return conflict(`a folder named "${name}" already exists`)
      }
      const folder = { id: takeId('folder'), name }
      state.folders.push(folder)
      recordAudit('folder.create', 'folder', folder.id, { name })
      return json(folder, 201)
    },
  ),

  // Jobs
  route('GET', '/api/v1/jobs', view, () => json({ jobs: state.jobs.map(jobSummary) })),

  route('GET', '/api/v1/jobs/:id', view, ({ params }) => {
    const job = findJob(params.id)
    return job ? json(jobDetail(job)) : notFound('job')
  }),

  // A path containing "broken" is read with problems and no good version (the job is not runnable).
  route(
    'POST',
    '/api/v1/jobs',
    manageJobs(({ body }) => ({ folderId: whole(fields(body).folder_id) })),
    ({ body }) => {
      const request = fields(body)
      const folderId = whole(request.folder_id)
      const repo = text(request.repo)
      const branch = text(request.branch)
      const path = text(request.path)
      if (folderId <= 0) {
        return invalid('folder_id is required')
      }
      if (!validRepo(repo)) {
        return invalid(`repo "${repo}" must look like owner/name`)
      }
      if (branch.trim() === '') {
        return invalid('branch is required')
      }
      if (path.trim() === '') {
        return invalid('path is required')
      }
      const folder = state.folders.find((candidate) => candidate.id === folderId)
      if (!folder) {
        return invalid(`folder ${folderId} does not exist`)
      }
      if (state.jobs.some((job) => job.repo === repo && job.branch === branch && job.path === path)) {
        return conflict(`${repo} on branch ${branch} at ${path} is already registered`)
      }
      const jobId = (path.split('/').pop() ?? path).replace(/\.ya?ml$/, '')
      const broken = path.includes('broken')
      const created: StoredJob = {
        id: takeId('job'),
        job_id: broken ? '' : jobId,
        title: broken ? '' : jobId,
        folder_id: folder.id,
        folder: folder.name,
        repo,
        branch,
        path,
        runnable: !broken,
        commit: broken ? '' : '5555555555555555555555555555555555555555',
        synced_at: broken ? null : now(),
        checked_at: now(),
        problem_count: broken ? 1 : 0,
        sync_error: '',
        sync_error_at: null,
        definition: broken
          ? null
          : buildJobDefinition(jobId, jobId, [{ id: 'all', label: 'All services', services: ['backend-api'], line: 6 }], []),
        source: broken ? '' : `apiVersion: yumdrake/v1\nkind: Job\njob: ${jobId}\n`,
        problems: broken ? [{ file: path, line: 3, message: 'labels must list at least one environment label' }] : [],
        problems_commit: broken ? '6666666666666666666666666666666666666666' : '',
      }
      state.jobs.push(created)
      recordAudit('job.register', 'job', created.id, { repo, branch, path })
      return json(jobDetail(created), 201)
    },
  ),

  route(
    'PATCH',
    '/api/v1/jobs/:id',
    manageJobs(({ params }) => ({ folderId: findJob(params.id)?.folder_id })),
    ({ params, body, me }) => {
      const job = findJob(params.id)
      if (!job) {
        return notFound('job')
      }
      const folderId = whole(fields(body).folder_id)
      if (folderId <= 0) {
        return invalid('folder_id is required')
      }
      const folder = state.folders.find((candidate) => candidate.id === folderId)
      if (!folder) {
        return invalid(`folder ${folderId} does not exist`)
      }
      if (me && !can(me, 'manage_jobs', { folderId })) {
        return errorResponse(403, 'forbidden', 'You do not have permission to do this')
      }
      job.folder_id = folder.id
      job.folder = folder.name
      recordAudit('job.move', 'job', job.id, { folder: folder.name })
      return json(jobDetail(job))
    },
  ),

  route(
    'POST',
    '/api/v1/jobs/:id/sync',
    manageJobs(({ params }) => ({ folderId: findJob(params.id)?.folder_id })),
    ({ params }) => {
      const job = findJob(params.id)
      if (!job) {
        return notFound('job')
      }
      job.checked_at = now()
      return json(jobDetail(job))
    },
  ),

  // Users
  route('GET', '/api/v1/admin/users', manageAccess, () => json({ users: state.users.map(userView) })),

  route('POST', '/api/v1/admin/users', manageAccess, ({ body }) => {
    const request = fields(body)
    const email = text(request.email).trim().toLowerCase()
    const name = text(request.name).trim()
    if (!email.includes('@') || name === '') {
      return invalid('email and name are required')
    }
    if (request.source !== 'local' && request.source !== 'service') {
      return invalid('source must be "local" or "service"')
    }
    if (text(request.password).length < 12) {
      return invalid('password must be at least 12 characters')
    }
    if (state.users.some((user) => user.email === email)) {
      return conflict(`a user with email "${email}" already exists`)
    }
    const user: AdminUser = {
      id: takeId('user'),
      email,
      name,
      source: request.source,
      disabled: false,
      groups: [],
      last_sign_in_at: null,
      created_at: now(),
    }
    state.users.push(user)
    recordAudit('user.create', 'user', user.id, { email, source: request.source })
    return json(user, 201)
  }),

  route('PATCH', '/api/v1/admin/users/:id', manageAccess, ({ params, body }) => {
    const user = state.users.find((candidate) => String(candidate.id) === params.id)
    if (!user) {
      return notFound('user')
    }
    const request = fields(body)
    const before = structuredClone(user)
    if (typeof request.disabled === 'boolean') {
      user.disabled = request.disabled
    }
    if (Array.isArray(request.group_ids)) {
      const ids = request.group_ids.map(Number)
      const unknown = ids.find((id) => !state.groups.some((group) => group.id === id))
      if (unknown !== undefined) {
        return invalid(`group ${unknown} does not exist`)
      }
      // Like the server, only the manual memberships are replaced; a group reached only through single sign-on stays.
      const manual = state.groups.filter((group) => ids.includes(group.id)).map((group) => ({ id: group.id, name: group.name, source: 'manual' as const }))
      const idpOnly = user.groups.filter((ref) => ref.source === 'idp' && !ids.includes(ref.id))
      user.groups = [...manual, ...idpOnly].sort((a, b) => a.id - b.id)
    }
    if (!hasAdministratorLeft()) {
      user.disabled = before.disabled
      user.groups = before.groups
      return conflict('at least one enabled user must remain in an admin group')
    }
    recordAudit('user.update', 'user', user.id, { disabled: user.disabled, groups: user.groups.map((ref) => ref.name) })
    return json(typeof request.disabled === 'boolean' && request.disabled ? { ...user, sessions_revoked: true } : user)
  }),

  // Groups
  route('GET', '/api/v1/admin/groups', manageAccess, () => json({ groups: state.groups.map(groupView) })),

  route('POST', '/api/v1/admin/groups', manageAccess, ({ body }) => {
    const request = fields(body)
    const name = text(request.name).trim()
    if (name === '') {
      return invalid('name is required')
    }
    if (state.groups.some((group) => group.name === name)) {
      return conflict(`a group named "${name}" already exists`)
    }
    const group: AdminGroup = { id: takeId('group'), name, admin: request.admin === true, member_count: 0 }
    state.groups.push(group)
    recordAudit('group.create', 'group', group.id, { name, admin: group.admin })
    return json(groupView(group), 201)
  }),

  route('PATCH', '/api/v1/admin/groups/:id', manageAccess, ({ params, body }) => {
    const group = state.groups.find((candidate) => String(candidate.id) === params.id)
    if (!group) {
      return notFound('group')
    }
    const before = group.admin
    group.admin = fields(body).admin === true
    if (!hasAdministratorLeft()) {
      group.admin = before
      return conflict('at least one enabled user must remain in an admin group')
    }
    recordAudit('group.update', 'group', group.id, { admin: group.admin })
    return json(groupView(group))
  }),

  // Identity provider group mappings
  route('GET', '/api/v1/admin/idp-mappings', manageAccess, () => json({ mappings: state.mappings })),

  route('GET', '/api/v1/admin/idp-mappings/:idpGroup', manageAccess, ({ params }) => {
    const mapping = state.mappings.find((candidate) => candidate.idp_group === decodeURIComponent(params.idpGroup))
    return mapping ? json(mapping) : notFound('mapping')
  }),

  route('PUT', '/api/v1/admin/idp-mappings/:idpGroup', manageAccess, ({ params, body }) => {
    const idpGroup = decodeURIComponent(params.idpGroup)
    const groupId = whole(fields(body).group_id)
    if (!state.groups.some((group) => group.id === groupId)) {
      return invalid(`group ${groupId} does not exist`)
    }
    const existing = state.mappings.find((mapping) => mapping.idp_group === idpGroup)
    if (existing) {
      existing.group_id = groupId
    } else {
      state.mappings.push({ idp_group: idpGroup, group_id: groupId })
    }
    recordAudit('idp_mapping.put', 'idp_mapping', idpGroup, { group_id: groupId })
    return json({ idp_group: idpGroup, group_id: groupId })
  }),

  route('DELETE', '/api/v1/admin/idp-mappings/:idpGroup', manageAccess, ({ params }) => {
    const idpGroup = decodeURIComponent(params.idpGroup)
    const index = state.mappings.findIndex((mapping) => mapping.idp_group === idpGroup)
    if (index < 0) {
      return notFound('mapping')
    }
    state.mappings.splice(index, 1)
    recordAudit('idp_mapping.delete', 'idp_mapping', idpGroup)
    return noContent()
  }),

  // Grants
  route('GET', '/api/v1/admin/grants', manageAccess, () => json({ grants: state.grants })),

  route('POST', '/api/v1/admin/grants', manageAccess, ({ body }) => {
    const request = fields(body)
    const action = text(request.action) as Action
    const groupId = whole(request.group_id)
    if (!ALL_ACTIONS.includes(action)) {
      return invalid(`action "${text(request.action)}" is not an action`)
    }
    if (!state.groups.some((group) => group.id === groupId)) {
      return invalid(`group ${groupId} does not exist`)
    }
    const grant = {
      id: takeId('grant'),
      group_id: groupId,
      action,
      label: text(request.label),
      environment: text(request.environment),
      folder_id: whole(request.folder_id),
    }
    state.grants.push(grant)
    recordAudit('grant.create', 'grant', grant.id, { group_id: groupId, action })
    return json(grant, 201)
  }),

  route('DELETE', '/api/v1/admin/grants/:id', manageAccess, ({ params }) => {
    const index = state.grants.findIndex((grant) => String(grant.id) === params.id)
    if (index < 0) {
      return notFound('grant')
    }
    state.grants.splice(index, 1)
    recordAudit('grant.delete', 'grant', params.id)
    return noContent()
  }),

  // Audit log
  route('GET', '/api/v1/admin/audit', { action: 'view_audit' }, ({ url }) => {
    const before = Number(url.searchParams.get('before') ?? '')
    const requested = Number(url.searchParams.get('limit') ?? '')
    const limit = Number.isFinite(requested) && requested > 0 ? Math.min(requested, 200) : 50
    const newestFirst = [...state.audit].sort((a, b) => b.id - a.id).filter((entry) => !(before > 0) || entry.id < before)
    const page = newestFirst.slice(0, limit)
    const hasMore = newestFirst.length > page.length
    return json({ entries: page, next_before: hasMore ? (page[page.length - 1]?.id ?? null) : null })
  }),

  // Single sign-on settings
  route('GET', '/api/v1/admin/sso', manageAccess, () => json(state.sso)),

  route('PUT', '/api/v1/admin/sso', manageAccess, ({ me, body }) => {
    // Changing the single sign-on settings is admin power, not just manage_access.
    if (!me?.admin) {
      return errorResponse(403, 'forbidden', 'only an administrator may change administrator access')
    }
    const request = fields(body)
    const metadata = text(request.idp_metadata_xml)
    if (request.enabled === true && !metadata.includes('<EntityDescriptor')) {
      return invalid('the identity provider metadata is not valid SAML metadata')
    }
    state.sso = {
      ...state.sso,
      enabled: request.enabled === true,
      idp_metadata_xml: metadata,
      // Empty means the default, which follows the public URL.
      entity_id: text(request.entity_id) || state.sso.entity_id_default,
      groups_attribute: text(request.groups_attribute) || 'groups',
      email_attribute: text(request.email_attribute),
      name_attribute: text(request.name_attribute) || 'displayName',
    }
    recordAudit('sso.update', 'sso', '', { enabled: state.sso.enabled })
    return json(state.sso)
  }),

  route('POST', '/api/v1/admin/sso/test', manageAccess, () => {
    const ok = state.sso.idp_metadata_xml.includes('<EntityDescriptor')
    return json({
      ok,
      message: ok
        ? 'The metadata is valid and has a signing certificate and a sign-on endpoint'
        : 'The metadata could not be read',
    })
  }),

  // The caller's own tokens
  route('GET', '/api/v1/tokens', 'session', ({ me }) => json({ tokens: state.tokens[me?.id ?? 0] ?? [] })),

  route('POST', '/api/v1/tokens', 'session', ({ me, body }) => {
    const request = fields(body)
    const name = text(request.name).trim()
    if (name === '') {
      return invalid('name is required')
    }
    const days = Math.min(whole(request.ttl_days) || 90, 365)
    const id = takeId('token')
    const created: CreatedToken = {
      id,
      name,
      created_at: now(),
      expires_at: new Date(Date.parse(now()) + days * 86_400_000).toISOString(),
      last_used_at: null,
      plaintext: `ymd_mock${String(id).padStart(4, '0')}abcdefghijklmnopqrstuvwxyz0123456789`,
    }
    const { plaintext: _plaintext, ...stored } = created
    void _plaintext
    const userId = me?.id ?? 0
    state.tokens[userId] = [...(state.tokens[userId] ?? []), stored]
    recordAudit('token.create', 'token', id, { name })
    return json(created, 201)
  }),

  route('DELETE', '/api/v1/tokens/:id', 'session', ({ me, params }) => {
    const userId = me?.id ?? 0
    const own = state.tokens[userId] ?? []
    if (!own.some((token) => String(token.id) === params.id)) {
      return notFound('token')
    }
    state.tokens[userId] = own.filter((token) => String(token.id) !== params.id)
    recordAudit('token.revoke', 'token', params.id)
    return noContent()
  }),

  // CLI device sign-in
  route('POST', '/api/v1/cli/login', 'public', () => {
    const count = Object.keys(state.devices).length + 1
    const deviceCode = `device-code-${count}`
    const userCode = `KQ7M2X${String(10 + count)}`
    state.devices[deviceCode] = { userCode, confirmedBy: null, tokenIssued: false }
    return json({
      device_code: deviceCode,
      user_code: userCode,
      verify_url: `https://yumdrake.example.com/cli/confirm?code=${userCode}`,
      interval: 5,
      expires_in: 600,
    })
  }),

  route('POST', '/api/v1/cli/login/confirm', 'session', ({ me, body }) => {
    const userCode = text(fields(body).user_code).trim().toUpperCase()
    const device = Object.values(state.devices).find((candidate) => candidate.userCode === userCode)
    if (!device) {
      return notFound('code')
    }
    device.confirmedBy = me
    recordAudit('cli.login.confirm', 'user', me?.id ?? 0)
    return noContent()
  }),

  route('POST', '/api/v1/cli/login/token', 'public', ({ body }) => {
    const device = state.devices[text(fields(body).device_code)]
    // Like the server: an unknown, used or timed-out code is one answer, 400 expired_token.
    if (!device || device.tokenIssued) {
      return errorResponse(400, 'expired_token', 'the device code is unknown or has expired')
    }
    if (!device.confirmedBy) {
      return json({ status: 'pending', interval: 5 })
    }
    device.tokenIssued = true
    const id = takeId('token')
    const expiresAt = new Date(Date.parse(now()) + 90 * 86_400_000).toISOString()
    const owner = device.confirmedBy.id
    state.tokens[owner] = [...(state.tokens[owner] ?? []), { id, name: 'yumdrake CLI', created_at: now(), expires_at: expiresAt, last_used_at: null }]
    return json({ status: 'complete', token: `ymd_mockcli${String(id).padStart(4, '0')}abcdefghijklmnopqrstuvwxyz0123456789`, token_id: id, expires_at: expiresAt })
  }),

  // ---- Runs ----

  route('GET', '/api/v1/runs', view, ({ me, url }) => {
    const params = url.searchParams
    const wanted = params.getAll('state')
    const unknown = wanted.find((name) => !runStateNames.includes(name as RunState))
    if (unknown) {
      return invalid(`unknown state "${unknown}"`)
    }
    const number = (name: string): number | undefined => {
      const raw = params.get(name)
      return raw === null || raw === '' ? undefined : Number(raw)
    }
    const job = number('job')
    const before = number('before')
    const limit = Math.min(number('limit') ?? 50, 200)
    if ([job, before, limit].some((value) => value !== undefined && (!Number.isInteger(value) || value < 1))) {
      return invalid('job, before and limit must be integers of at least 1')
    }
    const environment = params.get('environment') ?? ''
    const query = (params.get('q') ?? '').trim().toLowerCase()
    const matching = state.runs
      .filter((run) => job === undefined || run.job_source_id === job)
      .filter((run) => environment === '' || run.environment === environment)
      .filter((run) => wanted.length === 0 || wanted.includes(run.state))
      .filter((run) => before === undefined || run.id < before)
      .filter(
        (run) =>
          query === '' ||
          [String(run.id), run.job_id, run.label, run.environment, run.manifest_id].some((part) => part.toLowerCase().includes(query)),
      )
      .sort((a, b) => b.id - a.id)
      .slice(0, limit)
    void me
    const last = matching[matching.length - 1]
    const nextBefore = matching.length === limit && last ? last.id : null
    return json({ runs: matching.map(runRow), next_before: nextBefore })
  }),

  route('POST', '/api/v1/jobs/:id/runs', 'session', ({ me, params, body }) => {
    const job = findJob(params.id)
    if (!job) {
      return notFound('job')
    }
    const request = fields(body)
    const environment = text(request.environment)
    const known = state.inventory.environments.find((candidate) => candidate.name === environment)
    if (known && !can(me as Me, 'run', { label: known.label, environment, folderId: job.folder_id })) {
      return forbiddenResponse()
    }
    if (!job.definition) {
      return conflict('the job has no definition that can be run')
    }
    const selection = fields(request.selection)
    const problems = selectionProblems(job, environment, selection)
    if (problems.length > 0) {
      return json({ error: { code: 'invalid', message: 'the selection cannot be planned', problems } }, 422)
    }
    const run = createPlannedRun({
      job,
      environment,
      services: servicesOfSelection(job, selection),
      selection: request.selection as Selection,
      me: me as Me,
    })
    recordAudit('run.create', 'run', run.id, { environment })
    if (request.start === true) {
      run.auto_start = true
      startRun(run, me as Me)
    }
    return json(runView(run), 201)
  }),

  runRoute('GET', '/api/v1/runs/:id', 'view', (_ctx, run) => json(runView(run))),

  runRoute('GET', '/api/v1/runs/:id/plan', 'view', (_ctx, run) => {
    const plan = state.plans[run.id]
    return plan ? json(plan) : notFound('plan')
  }),

  runRoute('GET', '/api/v1/runs/:id/log', 'view_logs', ({ url }, run) => {
    const params = url.searchParams
    const level = params.get('level') ?? ''
    if (level !== '' && !['info', 'ok', 'warn', 'error'].includes(level)) {
      return invalid(`unknown level "${level}"`)
    }
    const step = params.get('step') ?? ''
    if (step !== '' && !run.steps.some((candidate) => candidate.name === step)) {
      return invalid(`the run has no step "${step}"`)
    }
    const raw = params.get('limit')
    const requested = raw === null || raw === '' ? 200 : Number(raw)
    if (!Number.isInteger(requested) || requested < 1) {
      return invalid('limit must be an integer of at least 1')
    }
    const limit = Math.min(requested, 1000)
    const after = params.get('after') ?? ''
    const match = /^c(\d+)$/.exec(after)
    if (after !== '' && !match) {
      return invalid('the after cursor is not one this log produced')
    }
    const start = match ? Number(match[1]) : 0
    const service = params.get('service') ?? ''
    const query = (params.get('q') ?? '').toLowerCase()
    const legOf = new Map(run.legs.map((leg) => [leg.id, leg]))
    const all = [...(state.logs[run.id] ?? [])]
      .sort((a, b) => a.at.localeCompare(b.at) || a.leg_id - b.leg_id || a.seq - b.seq)
      .filter((line) => {
        const leg = legOf.get(line.leg_id)
        if (step !== '' && leg?.step !== step) return false
        if (service !== '' && leg?.service !== service) return false
        if (level !== '' && line.level !== level) return false
        return query === '' || line.text.toLowerCase().includes(query)
      })
    const page = all.slice(start, start + limit)
    return json({ lines: page, next: page.length > 0 ? `c${start + page.length}` : after, gaps: state.logGaps[run.id] ?? [] })
  }),

  runRoute('POST', '/api/v1/runs/:id/start', 'run', (_ctx, run, me) => {
    if (run.state !== 'planned') {
      return wrongState('start', run)
    }
    startRun(run, me)
    recordAudit('run.start', 'run', run.id, { manifest_id: run.manifest_id, next_state: run.state })
    return json(runView(run))
  }),

  runRoute('POST', '/api/v1/runs/:id/cancel', 'cancel', (_ctx, run, me) => {
    if (finalStates.includes(run.state)) {
      return wrongState('cancel', run)
    }
    const wasState = run.state
    run.cancel_requested = true
    appendRunLine(run, 'warn', `Cancel requested by ${me.name} while ${wasState}.`)
    if (wasState === 'running' || wasState === 'cancelling') {
      moveRun(run, 'cancelling', 'Waiting for the services already rolling to finish')
    } else {
      for (const leg of run.legs) {
        if (leg.state === 'pending' || leg.state === 'queued') leg.state = 'cancelled'
      }
      run.locks = []
      moveRun(run, 'cancelled', `Cancelled by ${me.name}`)
    }
    refreshSummary(run)
    recordAudit('run.cancel', 'run', run.id, { state: wasState })
    return json(runView(run))
  }),

  runRoute('POST', '/api/v1/runs/:id/legs/:leg/reconcile', 'restore', ({ params, body }, run, me) => {
    const outcome = text(fields(body).outcome)
    if (outcome !== 'succeeded' && outcome !== 'failed') {
      return invalid('outcome must be "succeeded" or "failed"')
    }
    const leg = unknownLeg(run, params.leg)
    if (run.state !== 'needs_reconciliation') {
      return conflict(`run is not in a state that allows this: it is ${run.state}`)
    }
    if (!leg) {
      return notFound('run or leg')
    }
    if (leg.state !== 'unknown') {
      return conflict(`run is not in a state that allows this: the leg is ${leg.state}, not unknown`)
    }
    leg.state = outcome
    leg.finished_at = now()
    appendRunLine(run, outcome === 'succeeded' ? 'ok' : 'warn', `${me.name} marked ${leg.step} for ${leg.service} as ${outcome}.`)
    settleReconciledRun(run)
    refreshSummary(run)
    recordAudit('run.reconcile', 'run', run.id, { leg: leg.id, outcome })
    return json(runView(run))
  }),

  runRoute('POST', '/api/v1/runs/:id/legs/:leg/check-again', 'restore', ({ params }, run, me) => {
    const leg = unknownLeg(run, params.leg)
    if (run.state !== 'needs_reconciliation') {
      return conflict(`run is not in a state that allows this: it is ${run.state}`)
    }
    if (!leg) {
      return notFound('run or leg')
    }
    if (leg.state !== 'unknown') {
      return conflict(`run is not in a state that allows this: the leg is ${leg.state}, not unknown`)
    }
    leg.state = 'queued'
    leg.attempt += 1
    refreshSummary(run)
    appendRunLine(run, 'info', `${me.name} asked to check ${leg.step} for ${leg.service} again.`)
    recordAudit('run.check_again', 'run', run.id, { leg: leg.id })
    return json(runView(run))
  }),

  // The route needs manage_access; the engine then wants an administrator.
  route('POST', '/api/v1/admin/runs/:id/release-locks', manageAccess, ({ me, params }) => {
    const run = findRun(params.id)
    if (!run) {
      return notFound('run')
    }
    if (!me?.admin) {
      return forbiddenResponse()
    }
    if (run.state !== 'needs_reconciliation') {
      return wrongState('release locks of', run)
    }
    const active = run.legs.find((leg) => ['queued', 'claimed', 'running'].includes(leg.state))
    if (active) {
      return wrongState('release locks of', run, `: a leg is still ${active.state}`)
    }
    run.locks = []
    moveRun(run, 'failed', `locks released by ${me.name} without reconciliation`)
    appendRunLine(run, 'warn', `Locks released by ${me.name} without reconciliation.`)
    recordAudit('run.release_locks', 'run', run.id)
    return json(runView(run))
  }),

  runRoute('POST', '/api/v1/runs/:id/restore', 'restore', ({ body }, run, me) => {
    const changed = changedServices(run)
    if (!state.plans[run.id] || changed.length === 0 || !finalStates.includes(run.state)) {
      return wrongState('restore', run, ': nothing in it can be restored')
    }
    const asked = fields(body).services
    const services = Array.isArray(asked) && asked.length > 0 ? asked.map(text) : changed
    const stranger = services.find((service) => !changed.includes(service))
    if (stranger) {
      return invalid(`the run did not change service "${stranger}"`)
    }
    const job = state.jobs.find((candidate) => candidate.id === run.job_source_id)
    if (!job) {
      return notFound('job')
    }
    const restore = createPlannedRun({
      job,
      environment: run.environment,
      services: Object.fromEntries(services.map((service) => [service, serviceVersionsOf(run)[service] ?? ''])),
      stepNames: [
        { name: 'restore-config', use: 'copy-config' },
        { name: 'restore-deploy', use: 'ecs-deploy' },
      ],
      kind: 'restore',
      restoreOf: run.id,
      me,
    })
    recordAudit('run.restore', 'run', restore.id, { source: run.id })
    return json(runView(restore), 201)
  }),

  runRoute('POST', '/api/v1/runs/:id/rerun-failed', 'run', (_ctx, run, me) => {
    const broken = [...new Set(run.legs.filter((leg) => leg.kind === 'step' && ['failed', 'unknown', 'skipped', 'cancelled'].includes(leg.state)).map((leg) => leg.service))]
    const job = state.jobs.find((candidate) => candidate.id === run.job_source_id)
    if (broken.length === 0) {
      return wrongState('rerun', run, ': no service failed, was skipped or was cancelled')
    }
    if (!job) {
      return notFound('job')
    }
    const versions = serviceVersionsOf(run)
    const services = Object.fromEntries(broken.map((service) => [service, versions[service] ?? '']))
    const rerun = createPlannedRun({ job, environment: run.environment, services, selection: selectionOfServices(job, services), me })
    recordAudit('run.rerun_failed', 'run', rerun.id, { source: run.id })
    return json(runView(rerun), 201)
  }),

  runRoute('POST', '/api/v1/runs/:id/approve', 'approve', ({ body }, run, me) => {
    if (!approvalAbility(me, run).can) {
      return forbiddenResponse()
    }
    if (run.state !== 'waiting_approval' || run.approval?.decision !== 'pending') {
      return conflict('This run was already decided.')
    }
    const request = fields(body)
    if (text(request.manifest_id) !== run.manifest_id) {
      return conflict('The plan changed. Reload and review it again.')
    }
    run.approval = { ...run.approval, decision: 'approved', decided_by_name: me.name, decided_at: now(), comment: text(request.comment) }
    moveRun(run, 'queued', 'Waiting for its turn')
    appendRunLine(run, 'ok', `Approved by ${me.name}.`)
    recordAudit('run.approve', 'run', run.id, { manifest_id: run.manifest_id })
    return json(runView(run))
  }),

  runRoute('POST', '/api/v1/runs/:id/reject', 'approve', ({ body }, run, me) => {
    if (!approvalAbility(me, run).can) {
      return forbiddenResponse()
    }
    if (run.state !== 'waiting_approval' || run.approval?.decision !== 'pending') {
      return conflict('This run was already decided.')
    }
    run.approval = { ...run.approval, decision: 'rejected', decided_by_name: me.name, decided_at: now(), comment: text(fields(body).comment) }
    moveRun(run, 'rejected', `Rejected by ${me.name}`)
    appendRunLine(run, 'warn', `Rejected by ${me.name}.`)
    recordAudit('run.reject', 'run', run.id)
    return json(runView(run))
  }),

  route('GET', '/api/v1/approvals', view, ({ me }) => {
    const approvals: ApprovalItem[] = state.runs
      .filter((run) => run.state === 'waiting_approval' && run.approval)
      .sort((a, b) => b.id - a.id)
      .map((run) => {
        const ability = approvalAbility(me as Me, run)
        return {
          run_id: run.id,
          job_source_id: run.job_source_id,
          job_id: run.job_id,
          job_name: run.job_name || run.job_id,
          environment: run.environment,
          label: run.label,
          manifest_id: run.manifest_id,
          requested_by_name: run.approval?.requested_by_name ?? run.requested_by_name,
          requested_at: run.approval?.requested_at ?? run.created_at,
          expires_at: run.approval?.expires_at ?? '',
          versions: Object.entries(serviceVersionsOf(run)).map(([service, version]) => ({ service, version })),
          can_approve: ability.can,
          reason: ability.reason,
        }
      })
    return json({ approvals })
  }),

  // Versions of the images a job's services can be deployed at, for one environment.
  route('GET', '/api/v1/jobs/:id/versions', view, ({ params, url }) => {
    const job = findJob(params.id)
    if (!job) {
      return notFound('job')
    }
    if (!job.definition) {
      return conflict('the job has no definition that can be run')
    }
    const environment = url.searchParams.get('environment') ?? ''
    if (!state.inventory.environments.some((candidate) => candidate.name === environment)) {
      return notFound('environment')
    }
    return json(versionsOfJob(job, environment))
  }),

  // ---- Freezes ----

  route('GET', '/api/v1/freezes', view, () => {
    const active = state.freezes.filter(isActiveFreeze)
    const ended = state.freezes
      .filter((freeze) => !isActiveFreeze(freeze))
      .sort((a, b) => (b.ended_at ?? b.expires_at).localeCompare(a.ended_at ?? a.expires_at))
    return json({ active, ended })
  }),

  route('POST', '/api/v1/admin/freezes', manageAccess, ({ me, body }) => {
    const request = fields(body)
    const label = text(request.label).trim()
    const environment = text(request.environment).trim()
    const reason = text(request.reason).trim()
    if (reason === '') {
      return invalid('reason is required')
    }
    if ((label === '') === (environment === '')) {
      return invalid('give a label or an environment, not both and not neither')
    }
    if (environment !== '' && !state.inventory.environments.some((candidate) => candidate.name === environment)) {
      return invalid(`environment "${environment}" is not in the inventory`)
    }
    const expires = Date.parse(text(request.expires_at))
    if (Number.isNaN(expires) || expires <= Date.parse(now())) {
      return invalid('expires_at must be a time in the future')
    }
    if (expires > Date.parse(now()) + 30 * 86_400_000) {
      return invalid('a freeze lasts at most 30 days')
    }
    const freeze: Freeze = {
      id: takeId('freeze'),
      label,
      environment,
      reason,
      created_by_name: me?.name ?? '',
      created_at: now(),
      expires_at: new Date(expires).toISOString(),
      ended_at: null,
      ended_by_name: '',
    }
    state.freezes.push(freeze)
    recordAudit('freeze.create', 'freeze', freeze.id, { label, environment, reason })
    return json(freeze, 201)
  }),

  route('DELETE', '/api/v1/admin/freezes/:id', manageAccess, ({ me, params }) => {
    const freeze = state.freezes.find((candidate) => String(candidate.id) === params.id)
    if (!freeze) {
      return notFound('freeze')
    }
    if (!isActiveFreeze(freeze)) {
      return conflict('The freeze has already ended.')
    }
    freeze.ended_at = now()
    freeze.ended_by_name = me?.name ?? ''
    recordAudit('freeze.delete', 'freeze', freeze.id)
    return noContent()
  }),

  // ---- Slack ----

  route('GET', '/api/v1/admin/slack', manageConnections, () => json(state.slack)),

  route('PUT', '/api/v1/admin/slack', manageConnections, ({ me, body }) => {
    if (text(fields(body).token).trim() === '') {
      return invalid('token is required')
    }
    state.slack = { token_set: true, set_at: now(), set_by_name: me?.name ?? '' }
    recordAudit('slack.update', 'slack', '')
    return json(state.slack)
  }),

  // A channel whose name contains "missing" answers like Slack's channel_not_found.
  route('POST', '/api/v1/admin/slack/test', manageConnections, ({ body }) => {
    const channel = text(fields(body).channel).trim()
    if (channel === '') {
      return invalid('channel is required')
    }
    if (!state.slack.token_set) {
      return json({ ok: false, code: 'not_configured' })
    }
    return json(channel.includes('missing') ? { ok: false, code: 'channel_not_found' } : { ok: true, code: '' })
  }),

  // ---- Running versions and the service page (Plan 9) ----
  route('GET', '/api/v1/versions/running', view, ({ url, me }) => {
    if (url.searchParams.get('refresh') === '1' && me && !can(me, 'manage_connections')) {
      return errorResponse(403, 'forbidden', 'You do not have permission to do this')
    }
    return json(runningFixture())
  }),

  route('GET', '/api/v1/services/:name', view, ({ params }) => {
    const service = state.services.find((candidate) => candidate.name === params.name)
    return service ? json(servicePageFixture(service)) : notFound('service')
  }),

  // ---- Queue and system health (Plan 9) ----
  route('GET', '/api/v1/queue', view, () => json(queueFixture())),
  route('GET', '/api/v1/system/health', manageConnections, () => json(healthFixture())),

  // ---- Overview (Plan 9) ----
  route('GET', '/api/v1/analytics/overview', view, ({ url }) => {
    const days = Number(url.searchParams.get('days') ?? '14')
    return json(overviewFixture(days, url.searchParams.get('label') ?? ''))
  }),
]
