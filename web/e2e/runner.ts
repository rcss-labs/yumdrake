// Stops and restarts the runner process the webstack helper started (its id comes from global setup).
// The process is paused with SIGSTOP, so it stops heartbeating exactly as a dead runner does, and
// resumed with SIGCONT, so the helper can still stop it at the end. Always call resumeRunner in afterAll.

function runnerPid(): number {
  const pid = Number(process.env.YUMDRAKE_E2E_RUNNER_PID)
  if (!Number.isInteger(pid) || pid <= 0) throw new Error('the global setup gave no runner process id')
  return pid
}

export function stopRunner() {
  process.kill(runnerPid(), 'SIGSTOP')
}

export function resumeRunner() {
  try {
    process.kill(runnerPid(), 'SIGCONT')
  } catch {
    // The runner is already gone; nothing to resume.
  }
}
