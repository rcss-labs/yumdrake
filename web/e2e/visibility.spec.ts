import { expect, test, type Page } from '@playwright/test'
import { mkdirSync } from 'node:fs'
import { resolve } from 'node:path'
import { resumeRunner, stopRunner } from './runner'
import { stubApprovals, stubFreezes, stubRunningVersions } from './stubs'

// The visibility screens in a real browser: the real orchestrator, the real runner with fake steps, and no AWS
// (the orchestrator's AWS address is unreachable on purpose). Only GET /api/v1/versions/running is stubbed.
//
// This file must run last: it pauses the runner. Playwright runs files in alphabetical order, and "visibility"
// sorts after foundation and runs. afterAll resumes the runner whatever happens.

const baseURL = process.env.YUMDRAKE_E2E_BASE_URL ?? ''
const email = process.env.YUMDRAKE_E2E_EMAIL ?? ''
const password = process.env.YUMDRAKE_E2E_PASSWORD ?? ''
const environment = 'hetzner-01'
const service = 'backend-api'
const failure = 'health checks failed'

/** Screenshots are written only when YUMDRAKE_SHOTS_DIR is set, at two widths. */
async function shots(page: Page, name: string) {
  const dir = process.env.YUMDRAKE_SHOTS_DIR
  if (!dir) return
  mkdirSync(dir, { recursive: true })
  for (const width of [1360, 380]) {
    await page.setViewportSize({ width, height: width === 1360 ? 900 : 800 })
    await page.screenshot({ path: resolve(dir, `${name}-${width}.png`), fullPage: true, animations: 'disabled' })
  }
  await page.setViewportSize({ width: 1360, height: 900 })
}

/** Starts a run of the job at one version for the one service the environment has, through the API. */
async function startRun(page: Page, jobId: number, version: string): Promise<number> {
  const response = await page.request.post(`${baseURL}/api/v1/jobs/${jobId}/runs`, {
    headers: { Origin: baseURL },
    data: { environment, selection: { backends: { version, services: { [service]: '' } } }, start: true },
  })
  expect(response.status(), `starting a run at ${version}`).toBeLessThan(300)
  return ((await response.json()) as { id: number }).id
}

async function runState(page: Page, id: number): Promise<string> {
  const response = await page.request.get(`${baseURL}/api/v1/runs/${id}`)
  return ((await response.json()) as { state: string }).state
}

test.afterAll(() => {
  resumeRunner()
})

test('the overview, versions, service, queue and health screens tell the truth about real runs', async ({ page }) => {
  test.setTimeout(240_000)
  expect(baseURL, 'the global setup must provide the orchestrator address').not.toBe('')
  await stubApprovals(page)
  await stubFreezes(page)
  await stubRunningVersions(page)

  // Sign in. The password is set natively so no step title shows it.
  await page.goto(`${baseURL}/sign-in`)
  await page.getByLabel('Email').fill(email)
  await page.getByLabel('Password').evaluate((element, secret) => {
    const input = element as HTMLInputElement
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set?.call(input, secret)
    input.dispatchEvent(new Event('input', { bubbles: true }))
  }, password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).not.toHaveURL(/sign-in/)

  const jobs = (await (await page.request.get(`${baseURL}/api/v1/jobs`)).json()) as { jobs: { id: number; title: string }[] }
  const jobId = jobs.jobs.find((job) => job.title === 'Backend and microservices')?.id
  expect(jobId, 'the job of the fixture').toBeDefined()

  // ---- Two real runs: one clean, one with a failing service ----
  const cleanRun = await startRun(page, jobId!, '1.0.0')
  await expect.poll(() => runState(page, cleanRun), { timeout: 90_000, intervals: [1000] }).toBe('succeeded')
  const failedRun = await startRun(page, jobId!, '1.0.0-fail')
  await expect.poll(() => runState(page, failedRun), { timeout: 90_000, intervals: [1000] }).toBe('partially_succeeded')

  // ---- Overview ----
  await page.goto(`${baseURL}/`)
  await expect(page.getByRole('heading', { name: 'Overview', level: 1 })).toBeVisible()
  await expect(page.getByText('Success rate', { exact: true })).toBeVisible()
  await expect(page.getByRole('link', { name: new RegExp(`${service}|Backend`) }).first()).toBeVisible()
  const attention = page.getByRole('link', { name: new RegExp(`${environment}`) }).first()
  await expect(attention).toBeVisible()
  await expect(page.getByText(failure).first()).toBeVisible()
  await expect(page.getByRole('link', { name: `#${failedRun}` }).first()).toBeVisible()
  // The environments table names the newest run of hetzner-01, which is the failed one; the clean one shows
  // in the counts (at least the two runs of this spec) and on the service page below.
  const lastRuns = page.getByRole('table', { name: /environments/i })
  await expect(lastRuns.getByRole('row').filter({ hasText: environment }).getByRole('link', { name: `#${failedRun}` })).toBeVisible()
  await shots(page, 'overview')

  // ---- Versions: the stubbed matrix, a search and "Only differences" ----
  await page.goto(`${baseURL}/versions`)
  await expect(page.getByRole('heading', { name: 'Versions', level: 1 })).toBeVisible()
  const matrix = page.getByRole('table', { name: 'Running versions' })
  await expect(matrix.getByRole('link', { name: service })).toBeVisible()
  await expect(matrix.getByRole('link', { name: 'billing' })).toBeVisible()
  await expect(matrix.getByRole('link', { name: 'orders' })).toBeVisible()
  await expect(page.getByText('3 of 3 services')).toBeVisible()
  await shots(page, 'versions')
  await page.getByRole('searchbox', { name: 'Find a service' }).fill('ord')
  await expect(matrix.getByRole('link', { name: 'orders' })).toBeVisible()
  await expect(matrix.getByRole('link', { name: service })).toHaveCount(0)
  await expect(page.getByText('1 of 3 services')).toBeVisible()
  await page.getByRole('searchbox', { name: 'Find a service' }).fill('')
  await expect(page.getByText('3 of 3 services')).toBeVisible()
  // The box is controlled by the address, which the router updates a moment after the click: click, then wait.
  await page.getByLabel('Only differences').click()
  await expect(page.getByLabel('Only differences')).toBeChecked()
  await expect(matrix.getByRole('link', { name: 'orders' })).toHaveCount(0)
  await expect(matrix.getByRole('link', { name: service })).toBeVisible()
  await expect(matrix.getByRole('link', { name: 'billing' })).toBeVisible()
  await expect(page.getByText('2 of 3 services')).toBeVisible()

  // ---- A service page: its deployment history and a way to restore ----
  await page.goto(`${baseURL}/services/${service}`)
  await expect(page.getByRole('heading', { name: service, level: 1 })).toBeVisible()
  const deployments = page.getByRole('table', { name: 'Deployments' })
  await expect(deployments.getByRole('link', { name: `#${failedRun}` })).toBeVisible()
  await expect(deployments.getByRole('link', { name: `#${cleanRun}` })).toBeVisible()
  await expect(deployments).toContainText('1.0.0-fail')
  await expect(page.getByRole('button', { name: 'Restore' })).toBeVisible()
  await shots(page, 'service')

  // ---- Queue: a slow run holds the environment, so the next run waits for it ----
  const slowRun = await startRun(page, jobId!, '1.0.0-slow')
  const waitingRun = await startRun(page, jobId!, '1.0.0')
  await page.goto(`${baseURL}/queue`)
  await expect(page.getByRole('heading', { name: 'Queue', level: 1 })).toBeVisible()
  const runs = page.getByRole('table', { name: 'Active and waiting runs' })
  const waiting = runs.getByRole('row').filter({ has: page.getByRole('link', { name: `#${waitingRun}`, exact: true }) })
  await expect(waiting).toContainText(`Waiting for run #${slowRun}`, { timeout: 60_000 })
  await expect(page.getByText(/1 runner, \d of \d slots busy/)).toBeVisible()
  await shots(page, 'queue')

  // ---- System health: the runner stops, and the screen says so ----
  stopRunner()
  await expect
    .poll(async () => ((await (await page.request.get(`${baseURL}/api/v1/system/health`)).json()) as { problems: string[] }).problems, {
      timeout: 30_000,
      intervals: [1000],
    })
    .toContain('No runner has reported for 5 seconds.')
  await page.goto(`${baseURL}/admin/health`)
  await expect(page.getByText('No runner has reported for 5 seconds.').first()).toBeVisible()
  await shots(page, 'health')
})
