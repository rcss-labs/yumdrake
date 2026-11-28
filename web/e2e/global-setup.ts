import { execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { createInterface } from 'node:readline'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const repoRoot = resolve(here, '..', '..')
const helperPath = resolve(repoRoot, 'web', '.e2e-bin', 'webstack')
const startLimitMs = 120_000
const stopLimitMs = 40_000

interface Ready {
  base_url: string
  email: string
  password: string
  runner_pid: number
}

/** Waits for the helper's one JSON line; rejects if it exits first or takes too long. */
function readReady(child: ChildProcess): Promise<Ready> {
  return new Promise((resolveReady, reject) => {
    if (!child.stdout) {
      reject(new Error('the webstack helper has no standard output'))
      return
    }
    const timer = setTimeout(() => reject(new Error('the webstack helper was not ready in time')), startLimitMs)
    const lines = createInterface({ input: child.stdout })
    lines.once('line', (line) => {
      clearTimeout(timer)
      try {
        resolveReady(JSON.parse(line) as Ready)
      } catch {
        reject(new Error('the webstack helper printed something that is not JSON'))
      }
    })
    child.once('exit', (code) => {
      clearTimeout(timer)
      reject(new Error(`the webstack helper exited with code ${code ?? 'null'} before it was ready`))
    })
  })
}

function stop(child: ChildProcess): Promise<void> {
  return new Promise((resolveStop) => {
    if (child.exitCode !== null || child.signalCode !== null) {
      resolveStop()
      return
    }
    const killer = setTimeout(() => child.kill('SIGKILL'), stopLimitMs)
    child.once('exit', () => {
      clearTimeout(killer)
      resolveStop()
    })
    child.kill('SIGTERM')
  })
}

export default async function globalSetup(): Promise<() => Promise<void>> {
  // A helper binary (not `go run`) so signals reach the helper itself, which then stops the orchestrator.
  mkdirSync(dirname(helperPath), { recursive: true })
  execFileSync('go', ['build', '-o', helperPath, './test/e2e/webstack'], { cwd: repoRoot, stdio: 'inherit' })
  // The runner with fake steps only. The production runner wires the real resolve, which needs AWS.
  execFileSync('go', ['build', '-o', resolve(dirname(helperPath), 'webrunner'), './test/e2e/webrunner'], { cwd: repoRoot, stdio: 'inherit' })

  // The helper stops when its standard input closes, so it also stops if this process dies.
  const child = spawn(helperPath, [], { cwd: repoRoot, stdio: ['pipe', 'pipe', 'inherit'] })
  const onSignal = () => {
    void stop(child)
  }
  process.once('SIGINT', onSignal)
  process.once('SIGTERM', onSignal)
  process.once('exit', () => child.kill('SIGTERM'))

  let ready: Ready
  try {
    ready = await readReady(child)
  } catch (error) {
    await stop(child)
    throw error
  }
  process.env.YUMDRAKE_E2E_BASE_URL = ready.base_url
  process.env.YUMDRAKE_E2E_EMAIL = ready.email
  process.env.YUMDRAKE_E2E_PASSWORD = ready.password
  // The runner's process id, so a spec can stop and restart it (see e2e/runner.ts).
  process.env.YUMDRAKE_E2E_RUNNER_PID = String(ready.runner_pid)

  return async () => {
    await stop(child)
  }
}
