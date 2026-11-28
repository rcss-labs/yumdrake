import type { ApiError } from './error'

// Request and response shapes for every route of the API contract.
// Field names follow the Go structs in internal/api and internal/spec (they win over plan text);
// where the plans give none (access administration, tokens, CLI sign-in, SSO, AWS accounts)
// the names are snake_case in the same style. Timestamps are ISO 8601 strings.

// ---- Access ---------------------------------------------------------------

export type Action =
  | 'view'
  | 'view_logs'
  | 'run'
  | 'cancel'
  | 'approve'
  | 'restore'
  | 'manage_jobs'
  | 'manage_connections'
  | 'manage_access'
  | 'view_audit'

export const ALL_ACTIONS: readonly Action[] = [
  'view',
  'view_logs',
  'run',
  'cancel',
  'approve',
  'restore',
  'manage_jobs',
  'manage_connections',
  'manage_access',
  'view_audit',
]

/** A grant as the server sends it: an empty label or environment, or folder_id 0, means "any". */
export interface Grant {
  action: Action
  label: string
  environment: string
  folder_id: number
}

/** GET /api/v1/me and the body of a successful POST /api/v1/auth/login. */
export interface Me {
  id: number
  email: string
  name: string
  admin: boolean
  grants: Grant[]
}

/** GET /api/v1/auth/methods */
export interface AuthMethods {
  password: boolean
  saml: boolean
}

/** POST /api/v1/auth/login */
export interface LoginRequest {
  email: string
  password: string
}

// ---- Problems and definitions (internal/spec) -------------------------------
// Slices in spec structs are built by appending to nil, so the server sends null when empty:
// every list of a parsed definition or service below is `T[] | null`. Lists in the API's own
// response structs (inventory, jobs, folders, problems) are never null.

export interface Problem {
  file: string
  /** 0 when the problem is about the whole file. */
  line: number
  message: string
}

export interface EnvironmentSummary {
  name: string
  label: string
  aws_account: string
}

export interface ServiceSummary {
  name: string
  /** Names of the environments that have a block for this service. */
  environments: string[]
}

/** The rejected newer commit of the inventory repository; the active commit stays in use. */
export interface RejectedInventory {
  commit: string
  problems: Problem[]
}

/** GET /api/v1/inventory, POST /api/v1/inventory/sync and PUT /api/v1/admin/inventory-source. */
export interface Inventory {
  /** Empty before the first activation. */
  commit: string
  activated_at: string | null
  checked_at: string | null
  sync_error: string
  sync_error_at: string | null
  rejected: RejectedInventory | null
  environments: EnvironmentSummary[]
  services: ServiceSummary[]
}

export interface EnvFile {
  name: string
  source: string
  destination: string
  line: number
}

export interface Toml {
  name: string
  source: string
  destination: string
  line: number
}

export interface ServiceEnvironment {
  ecs_service: string
  ecr_repository: string
  /** The Go server sends null, not [], when the file has none. */
  env_files: EnvFile[] | null
  tomls: Toml[] | null
  line: number
}

export interface VersionRule {
  pattern: string
  sort: string
  limit: number
}

/** GET /api/v1/inventory/services/{name} */
export interface Service {
  file: string
  name: string
  versions: VersionRule
  environments: Record<string, ServiceEnvironment>
  line: number
  /** The text of the service's file at the active commit; "" when none is stored. */
  source: string
}

export interface JobNeed {
  step: string
  scope: string
  line: number
}

export interface JobStep {
  name: string
  use: string
  group: string
  /** 0 means not set. */
  max_parallel: number
  /** Nanoseconds (Go time.Duration); 0 means not set. */
  timeout: number
  /** null when the step needs nothing (the Go server leaves the list nil). */
  needs: JobNeed[] | null
  line: number
}

export interface JobGroup {
  id: string
  label: string
  services: string[] | null
  line: number
}

export interface JobApproval {
  labels: string[] | null
  /** Nanoseconds (Go time.Duration). */
  expires: number
  line: number
}

/** The parsed job definition (last good version). */
export interface JobDefinition {
  file: string
  id: string
  title: string
  labels: string[] | null
  labels_line: number
  groups: JobGroup[] | null
  approval: JobApproval | null
  slack_channel: string
  steps: JobStep[] | null
}

// ---- Folders and jobs -------------------------------------------------------

export interface Folder {
  id: number
  name: string
}

export interface FoldersResponse {
  folders: Folder[]
}

export interface CreateFolderRequest {
  name: string
}

export interface JobSummary {
  id: number
  job_id: string
  title: string
  folder_id: number
  folder: string
  repo: string
  branch: string
  path: string
  /** False until a good definition has been read; the job cannot be started then. */
  runnable: boolean
  /** The last good commit (empty when there is none). */
  commit: string
  synced_at: string | null
  checked_at: string | null
  problem_count: number
  sync_error: string
  sync_error_at: string | null
  /** How many distinct services the last good definition deploys across its groups; 0 when it was never good. */
  services: number
  /** Environment labels whose runs need approval (empty when none do or the job was never good). */
  approval_labels: string[]
}

export interface JobsResponse {
  jobs: JobSummary[]
}

/** GET /api/v1/jobs/{id}, and the answer of POST /jobs, PATCH /jobs/{id} and POST /jobs/{id}/sync. */
export interface JobDetail extends JobSummary {
  /** The last good definition; null until the first good read. */
  definition: JobDefinition | null
  /** The YAML text of the last good commit; "" when none is stored. */
  source: string
  problems: Problem[]
  /** The commit the problems were found in. */
  problems_commit: string
}

export interface RegisterJobRequest {
  folder_id: number
  repo: string
  branch: string
  path: string
}

export interface MoveJobRequest {
  folder_id: number
}

/** GET /api/v1/admin/inventory-source: empty strings when none is stored. PUT takes the same fields. */
export interface InventorySource {
  repo: string
  branch: string
  path: string
}

export type InventorySourceRequest = InventorySource

// ---- GitHub connections -----------------------------------------------------

export type GithubMethod = 'token' | 'app'

export interface GithubConnection {
  id: number
  owner: string
  method: GithubMethod
  app_id: number
  installation_id: number
  created_at: string
}

export interface GithubConnectionsResponse {
  connections: GithubConnection[]
}

/** Method "token" takes only token; method "app" takes app_id, installation_id and private_key. */
export interface CreateGithubConnectionRequest {
  owner: string
  method: GithubMethod
  token?: string
  app_id?: number
  installation_id?: number
  private_key?: string
}

// ---- AWS accounts (Plan 5, Task 2) -----------------------------------------------

export type AwsMethod = 'role' | 'key'

/** What the server shows of a credential: never a secret; keys only by their last 4 characters. */
export interface AwsCredentialView {
  method: AwsMethod
  role_arn: string
  external_id: string
  access_key_id_last4: string
}

export interface AwsAccount {
  id: number
  name: string
  account_id: string
  discovery: AwsCredentialView
  deploy: AwsCredentialView
  /** Names of the environments that use this account. */
  environments: string[]
  created_at: string
}

export interface AwsAccountsResponse {
  accounts: AwsAccount[]
}

/** Method "role" takes role_arn and optional external_id; method "key" takes access_key_id and secret_access_key. */
export interface AwsCredentialInput {
  method: AwsMethod
  role_arn?: string
  external_id?: string
  access_key_id?: string
  secret_access_key?: string
}

export interface CreateAwsAccountRequest {
  name: string
  account_id: string
  discovery: AwsCredentialInput
  deploy: AwsCredentialInput
}

/** POST /api/v1/admin/aws-accounts/{id}/test */
export interface AwsAccountTestResult {
  ok: boolean
  /** The AWS error code when ok is false (for example "AccessDenied"). */
  error_code: string
  message: string
}

// ---- Access administration (Plan 3, Task 6) ---------------------------------

export type UserSource = 'local' | 'saml' | 'service'

export interface GroupRef {
  id: number
  name: string
  /** "manual" when an administrator put the user in the group (also when both ways); "idp" when only single sign-on did. */
  source: 'manual' | 'idp'
}

export interface AdminUser {
  id: number
  email: string
  name: string
  source: UserSource
  disabled: boolean
  groups: GroupRef[]
  last_sign_in_at: string | null
  created_at: string
  /** Only in the answer to a disable: false means the open sessions could not be ended (the user is refused anyway). */
  sessions_revoked?: boolean
}

export interface UsersResponse {
  users: AdminUser[]
}

export interface CreateUserRequest {
  email: string
  name: string
  source: 'local' | 'service'
  /** At least 12 characters. */
  password: string
}

/** Disable or enable, and/or replace the manual group memberships. */
export interface UpdateUserRequest {
  disabled?: boolean
  group_ids?: number[]
}

export interface AdminGroup {
  id: number
  name: string
  admin: boolean
  member_count: number
}

export interface GroupsResponse {
  groups: AdminGroup[]
}

export interface CreateGroupRequest {
  name: string
  admin?: boolean
}

export interface UpdateGroupRequest {
  admin: boolean
}

export interface IdpMapping {
  idp_group: string
  group_id: number
}

export interface IdpMappingsResponse {
  mappings: IdpMapping[]
}

export interface PutIdpMappingRequest {
  group_id: number
}

export interface AdminGrant extends Grant {
  id: number
  group_id: number
}

export interface GrantsResponse {
  grants: AdminGrant[]
}

/** Scopes left out or empty (label "", environment "", folder_id 0) match anything. */
export interface CreateGrantRequest {
  group_id: number
  action: Action
  label?: string
  environment?: string
  folder_id?: number
}

export interface AuditEntry {
  id: number
  at: string
  /** The actor's email, or "system". */
  actor: string
  /** Null for the system or when the user was deleted. */
  actor_id: number | null
  /** For example "grant.create" or "auth.login". */
  action: string
  target_type: string
  target_id: string
  /** Safe values only; secrets are never recorded. Absent when the entry has none. */
  detail?: Record<string, unknown>
}

/** GET /api/v1/admin/audit?before=&limit= (newest first, limit at most 200). */
export interface AuditResponse {
  entries: AuditEntry[]
  /** Pass as `before` for the next page; null on the last page. */
  next_before: number | null
}

// ---- Single sign-on ---------------------------------------------------------

export interface SsoSettings {
  enabled: boolean
  idp_metadata_xml: string
  entity_id: string
  groups_attribute: string
  email_attribute: string
  name_attribute: string
  /** Read-only: the addresses to give the identity provider. */
  acs_url: string
  metadata_url: string
  /** Read-only: the entity ID in use while `entity_id` is saved empty; it follows the public URL. */
  entity_id_default: string
}

/** An empty `entity_id` means "use the default". */
export type UpdateSsoRequest = Omit<SsoSettings, 'acs_url' | 'metadata_url' | 'entity_id_default'>

export interface SsoTestResult {
  ok: boolean
  message: string
}

// ---- Tokens and CLI sign-in --------------------------------------------------

export interface ApiToken {
  id: number
  name: string
  created_at: string
  /** Null for a token that does not expire. */
  expires_at: string | null
  last_used_at: string | null
}

export interface TokensResponse {
  tokens: ApiToken[]
}

export interface CreateTokenRequest {
  name: string
  /** Lifetime in days; the server defaults to 90 and caps at 365. */
  ttl_days?: number
}

/** The plaintext is shown once, on creation. */
export interface CreatedToken extends ApiToken {
  plaintext: string
}

/** POST /api/v1/cli/login (no sign-in needed). */
export interface DeviceLoginStart {
  device_code: string
  user_code: string
  verify_url: string
  /** Seconds between polls. */
  interval: number
  /** Seconds until the codes stop working. */
  expires_in: number
}

/** POST /api/v1/cli/login/confirm (signed in). */
export interface DeviceLoginConfirmRequest {
  user_code: string
}

/** POST /api/v1/cli/login/token */
export interface DeviceLoginPollRequest {
  device_code: string
}

/**
 * Pending until the person confirms the code; then `complete` once, with the token. A used or unknown code is
 * a 400 with code `expired_token`.
 */
export type DeviceLoginPollResponse =
  | { status: 'pending'; interval: number }
  | { status: 'complete'; token: string; token_id: number; expires_at: string }

/** The body of every error response. */
export interface ErrorBody {
  error: { code: string; message: string }
}

// ---- Runs (Plan 4, internal/api/runs.go and events.go) -------------------------------------
// The Go structs win over plan text. Lists are never null after the client's normaliser has run.
// Fields marked "assumed" are not served by Go yet; they are optional so a screen shows "Not available"
// (never crashes) until the server sends them.

export type RunKind = 'deploy' | 'restore'

export type RunState =
  | 'resolving'
  | 'planned'
  | 'waiting_approval'
  | 'queued'
  | 'running'
  | 'cancelling'
  | 'needs_reconciliation'
  | 'succeeded'
  | 'partially_succeeded'
  | 'failed'
  | 'cancelled'
  | 'rejected'
  | 'expired'
  | 'invalidated'

/** The states a run never leaves. */
export const FINAL_RUN_STATES: readonly RunState[] = [
  'succeeded',
  'partially_succeeded',
  'failed',
  'cancelled',
  'rejected',
  'expired',
  'invalidated',
]

export const isFinalRunState = (state: RunState): boolean => FINAL_RUN_STATES.includes(state)

export type LegState =
  | 'pending'
  | 'queued'
  | 'claimed'
  | 'running'
  | 'succeeded'
  | 'failed'
  | 'skipped'
  | 'cancelled'
  | 'unknown'

export type LegKind = 'resolve' | 'revalidate' | 'step'

export interface RunStep {
  id: number
  name: string
  use: string
  group: string
  position: number
  max_parallel: number
}

export interface RunLeg {
  id: number
  /** null for the resolve and revalidate legs. */
  step_id: number | null
  /** "" when step_id is null. */
  step: string
  kind: LegKind
  service: string
  version: string
  state: LegState
  attempt: number
  cancel_requested: boolean
  failure_short: string
  failure_detail: string
  queued_at: string | null
  started_at: string | null
  finished_at: string | null
}

export interface RunFailure {
  leg_id: number
  service: string
  step: string
  kind: LegKind
  /** "failed" or "unknown". */
  state: LegState
  short: string
  detail: string
}

export interface RunSummaryCounts {
  /** Has an entry for every leg state, zero included. */
  counts: Record<LegState, number>
  failures: RunFailure[]
}

/** One service of a run row (list rows only). */
export interface RunServiceChip {
  service: string
  version: string
  state: LegState
}

/** What a run row and the run detail share. */
export interface RunBase {
  id: number
  kind: RunKind
  job_source_id: number
  job_id: string
  environment: string
  label: string
  state: RunState
  wait_reason: string
  end_reason: string
  manifest_id: string
  requested_by: number
  via: string
  created_at: string
  state_changed_at: string
  started_at: string | null
  finished_at: string | null
  /** The job source's title; "" when it has none. */
  job_name: string
  /** Who asked: their name, else their email. */
  requested_by_name: string
  /** Assumed: for a restore run, the run it undoes. */
  restore_of?: number | null
}

/** GET /api/v1/runs rows. */
export interface RunSummary extends RunBase {
  /** One entry per service of the run's step legs, in order of first appearance, and where it stands. */
  services: RunServiceChip[]
  /** The same shape as the detail's summary. */
  summary: RunSummaryCounts
}

/** The group choice of a run: the same shape as the body of POST /jobs/{id}/runs. */
export interface GroupSelection {
  version: string
  /** Per-service overrides of the group's version. */
  services?: Record<string, string>
}
export type Selection = Record<string, GroupSelection>

export type ApprovalDecision = 'pending' | 'approved' | 'rejected' | 'expired'

/**
 * The open or last approval of a run. The first five fields are the command-line tool's shape (`runApproval`);
 * the optional ones are web extras. Whether the viewer may decide it comes from GET /approvals, not from here.
 */
export interface Approval {
  requested_by: number
  requested_at: string
  expires_at: string
  decision: ApprovalDecision
  comment: string
  requested_by_name?: string
  decided_by_name?: string | null
  decided_at?: string | null
}

/** GET /api/v1/runs/{id}, and the answer of every run action. */
export interface RunDetail extends RunBase {
  aws_account: string
  folder_id: number
  inventory_commit: string
  job_commit: string
  approval_required: boolean
  auto_start: boolean
  steps: RunStep[]
  cancel_requested: boolean
  legs: RunLeg[]
  /** Names of the locks the run holds. */
  locks: string[]
  summary: RunSummaryCounts
  /** Assumed: what was chosen, so Plan again and Re-run can repeat it. */
  selection?: Selection
  /** Assumed: null or absent when the run never asked for approval. */
  approval?: Approval | null
  /** Assumed: who cancelled the run; absent until the API sends it. */
  cancelled_by_name?: string
}

export interface RunsResponse {
  runs: RunSummary[]
  next_before: number | null
}

/** Filters of GET /api/v1/runs; `state` may repeat. */
export interface RunsFilter {
  job?: number
  environment?: string
  q?: string
  state?: RunState[]
  before?: number
  limit?: number
}

/** The body of the stream's `state` event: the changing part of a RunDetail (no steps). */
export interface RunStateEvent {
  id: number
  state: RunState
  wait_reason: string
  end_reason: string
  state_changed_at: string
  started_at: string | null
  finished_at: string | null
  cancel_requested: boolean
  legs: RunLeg[]
  locks: string[]
  summary: RunSummaryCounts
}

export type LogLevel = 'info' | 'ok' | 'warn' | 'error'

/** A line as the API sends it; leg_id 0 is a run-level line. */
export interface ApiLogLine {
  leg_id: number
  seq: number
  at: string
  level: LogLevel
  text: string
}

export interface ApiLogGap {
  leg_id: number
  from_seq: number
  to_seq: number
}

/** GET /api/v1/runs/{id}/log, and the data of a stream `log` event. */
export interface LogPage {
  lines: ApiLogLine[]
  next: string
  gaps: ApiLogGap[]
}

/** A log line as the screens use it (mapped once from ApiLogLine). legId 0 is a run-level line. */
export interface LogLine {
  runId: number
  legId: number
  seq: number
  at: string
  level: LogLevel
  text: string
}

export interface LogGap {
  legId: number
  fromSeq: number
  toSeq: number
}

export type RunConnection = 'live' | 'reconnecting' | 'finished' | 'no-access'

export interface RunLive {
  /** Steps, legs, summary, wait reason, approval, locks. */
  run: RunDetail | undefined
  /** Ordered by time, then leg, then seq. */
  lines: LogLine[]
  gaps: LogGap[]
  connection: RunConnection
  /** True when more than 20,000 lines arrived and the oldest were dropped. */
  truncated: boolean
  error: ApiError | undefined
  /** Fetches the run again and replaces it (lines are kept). Call it after a 409 to show what changed. */
  reload(): void
  /** Starts again after `error`: reads the run and its log again, or reopens the stream. */
  retry(): void
}

/** POST /api/v1/jobs/{id}/runs. */
export interface CreateRunRequest {
  environment: string
  selection: Selection
  start: boolean
}

/** The body of the 422 answer for a selection that cannot be planned. */
export interface SelectionProblemsBody {
  error: { code: string; message: string; problems: Problem[] }
}

export interface ReconcileRequest {
  outcome: 'succeeded' | 'failed'
}

/** POST /runs/{id}/start sends the manifest the person looked at (Plan 7 contract; Go ignores it today). */
export interface StartRunRequest {
  manifest_id: string
}

export interface RestoreRequest {
  /** Left out or empty: every service the run changed. */
  services?: string[]
}

export interface ApproveRequest {
  manifest_id: string
  comment?: string
}

export interface RejectRequest {
  comment?: string
}

// ---- Versions (Plan 5 Task 4, assumed) -------------------------------------------------

export interface ImageVersion {
  tag: string
  digest: string
  pushed_at: string
}

export interface GroupVersions {
  /** Tags present for every service, in the order of the first service's list. */
  common: string[]
  /** service -> its versions, newest first. */
  services: Record<string, ImageVersion[]>
  /** service -> why its versions could not be listed (not in this environment, or an AWS error code). */
  unavailable: Record<string, string>
}

/** GET /api/v1/jobs/{id}/versions?environment= */
export interface JobVersions {
  groups: Record<string, GroupVersions>
}

// ---- The plan (Plan 5 Tasks 6 and 9, assumed) ---------------------------------------------

/** 'unknown': the plan failed before it could tell. */
export type ConfigChange = 'new' | 'changed' | 'unchanged' | 'unknown'

/** Key names only; a value never travels. */
export interface KeyDiff {
  added: string[]
  removed: string[]
  changed: string[]
}

export interface ConfigItem {
  kind: 'env' | 'toml'
  name: string
  source: string
  destination: string
  /** env only. */
  branch?: string
  commit?: string
  /** env only: the dev branch was used because the version's branch does not exist. */
  fallback?: boolean
  source_version_id?: string
  sha256: string
  size: number
  previous_exists: boolean
  previous_sha256?: string
  change: ConfigChange
  /** env only, when changed. */
  keys?: KeyDiff | null
  /** Plan 6 (restore): why this item is left alone; "" or absent otherwise. */
  not_restored?: string
}

export interface PlanService {
  name: string
  group: string
  version: string
  image_digest: string
  ecs_service: string
  ecr_repository: string
  previous_task_definition: string
  /** "" when unknown. */
  previous_version: string
  /** Plan 6 (restore). */
  target_task_definition?: string
  config: ConfigItem[]
}

export type CheckStatus = 'passed' | 'failed' | 'not_verified'

export interface Check {
  name: string
  service: string
  status: CheckStatus
  detail: string
}

/** GET /api/v1/runs/{id}/plan: the redacted manifest. */
export interface Plan {
  id: string
  run_id: number
  environment: string
  label: string
  aws_account: string
  inventory_commit: string
  job_commit: string
  services: PlanService[]
  checks: Check[]
  /** Plan 6: the run this restore undoes. */
  restore_of?: number
}

// ---- Approvals (Plan 7 Task 2, assumed) -----------------------------------------------------

/** A row of GET /approvals. The names follow the command-line tool (`approvalItem`); the optional ones are web extras. */
export interface ApprovalItem {
  run_id: number
  job_id: string
  environment: string
  requested_by_name: string
  requested_at: string
  expires_at: string
  versions: { service: string; version: string }[]
  can_approve: boolean
  reason: string
  job_source_id?: number
  job_name?: string
  label?: string
  manifest_id?: string
}

export interface ApprovalsResponse {
  approvals: ApprovalItem[]
}

// ---- Freezes (Plan 7 Task 3, assumed) --------------------------------------------------------

export interface Freeze {
  id: number
  /** "" when the freeze is for an environment. */
  label: string
  /** "" when the freeze is for a label. */
  environment: string
  reason: string
  created_by_name: string
  created_at: string
  expires_at: string
  /** Set on an ended freeze: when and by whom it was lifted ("" when it simply expired). */
  ended_at: string | null
  ended_by_name: string
}

export interface FreezesResponse {
  active: Freeze[]
  ended: Freeze[]
}

export interface CreateFreezeRequest {
  label?: string
  environment?: string
  reason: string
  /** ISO 8601, at most 30 days ahead. */
  expires_at: string
}

// ---- Slack (Plan 7 Task 5, assumed) ------------------------------------------------------------

export interface SlackSettings {
  token_set: boolean
  set_at: string | null
  set_by_name: string
  /** True when Slack refused the token: nothing is sent until a new one is stored. */
  token_invalid?: boolean
  token_invalid_since?: string | null
}

export interface SlackTestResult {
  ok: boolean
  /** Slack's error code when ok is false (for example "channel_not_found"); "" otherwise. */
  code: string
}

// ---- Overview (Plan 9) -------------------------------------------------------

export interface OverviewDay {
  /** "2026-11-28". */
  date: string
  succeeded: number
  /** Includes partly succeeded runs. */
  failed: number
  /** Cancelled and invalidated runs. */
  other: number
}

export interface OverviewFailingService {
  service: string
  failures: number
  common_reason: string
}

export interface OverviewAttention {
  run_id: number
  state: RunState
  job: string
  environment: string
  label: string
  reason: string
  by: string
  at: string
}

export interface OverviewEnvironment {
  environment: string
  label: string
  run_id: number | null
  state: RunState
  job: string
  at: string | null
}

/** GET /api/v1/analytics/overview?label=&days=. */
export interface Overview {
  from: string
  to: string
  runs: number
  prev_runs: number
  /** 0..1, null when no run finished in the period. */
  success_rate: number | null
  prev_success_rate: number | null
  median_duration_seconds: number | null
  prev_median_duration_seconds: number | null
  waiting_approval: number
  oldest_waiting: string | null
  days: OverviewDay[]
  top_failing: OverviewFailingService[]
  attention: OverviewAttention[]
  environments: OverviewEnvironment[]
}

// ---- Running versions and the service page (Plan 9) -----------------------------

/** One service in one environment, as read from ECS. */
export interface RunningCell {
  state: 'ok' | 'error' | 'absent'
  /** "" when ECS runs it but no version could be worked out. */
  version: string
  /** "family:revision". */
  task_definition: string
  /** False when no Yumdrake run deployed what is running. */
  by_yumdrake: boolean
  /** The run that deployed it; 0 when by_yumdrake is false. */
  run_id: number
  last_deploy_failed: boolean
  /** Another environment with the same label runs a newer version. */
  behind: boolean
  /** The AWS error code when state is "error", otherwise "". */
  error: string
  read_at: string
}

export interface RunningRow {
  service: string
  /** One cell per environment of the matrix, in the same order. */
  cells: RunningCell[]
}

/** GET /api/v1/versions/running (?refresh=1 reads ECS again; needs manage_connections). */
export interface RunningMatrix {
  environments: EnvironmentSummary[]
  rows: RunningRow[]
}

export interface ServiceEnvironmentRunning {
  environment: string
  label: string
  cell: RunningCell
  /** The run a restore of this service in this environment would start from, if any. */
  restore_from_run_id: number | null
  /** The folder of that run, for a folder-scoped restore grant; null with restore_from_run_id. */
  restore_from_folder_id: number | null
}

export interface ServiceDeployment {
  run_id: number
  leg_id: number
  environment: string
  version: string
  state: LegState
  at: string
  by: string
}

export interface ServiceConfigItem {
  kind: string
  name: string
  destination: string
  /** The first 12 characters. */
  checksum: string
}

export interface ServiceConfigChange {
  run_id: number
  environment: string
  at: string
  items: ServiceConfigItem[]
}

/** GET /api/v1/services/{name}. */
export interface ServicePage {
  service: Service
  environments: ServiceEnvironmentRunning[]
  deployments: ServiceDeployment[]
  config: ServiceConfigChange[]
}

export interface QueueRun {
  id: number
  job_id: string
  job_name: string
  environment: string
  label: string
  state: RunState
  /** In words, for example "waiting for run #41". */
  wait_reason: string
  /** Parsed from a wait reason of the form "waiting for run #N". */
  blocked_by_run_id: number | null
  waiting_since: string
  waited_seconds: number
  legs_running: number
  legs_queued: number
}

export interface QueueLock {
  resource: string
  run_id: number
  acquired_at: string
}

export interface RunnerInfo {
  id: string
  version: string
  concurrency: number
  busy: number
  started_at: string
  last_seen_at: string
}

/** GET /api/v1/queue. */
export interface QueueView {
  runs: QueueRun[]
  locks: QueueLock[]
  runners: RunnerInfo[]
  free_slots: number
  queued_legs: number
  oldest_queued_at: string | null
  runner_gone_after_seconds: number
}

export interface SyncStatus {
  checked_at: string | null
  error: string
  error_at: string | null
}

export interface SyncJobStatus extends SyncStatus {
  id: number
  job_id: string
  title: string
}

export interface ConnectionHealth {
  kind: 'aws' | 'github' | 'slack'
  name: string
  last_ok_at: string | null
  last_error_at: string | null
  last_error_code: string
  failing: boolean
}

export interface HealthCounter {
  hour: number
  day: number
}

/** GET /api/v1/system/health (needs manage_connections). */
export interface SystemHealth {
  /** Plain-words problems, in order; empty when healthy. */
  problems: string[]
  runners: RunnerInfo[]
  runner_gone_after_seconds: number
  queue: { queued_legs: number; oldest_queued_at: string | null; active_runs: number }
  counters: { lease_expired: HealthCounter; log_persist_failed: HealthCounter; sync_failed: HealthCounter }
  sync: { inventory: SyncStatus; jobs: SyncJobStatus[] }
  connections: ConnectionHealth[]
  notifications: { pending: number; failed_last_day: number; oldest_pending_at: string | null }
}
