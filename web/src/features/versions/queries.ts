import { useApiGet } from '../../api/hooks'
import type { RunningMatrix } from '../../api/types'

export const runningKey = ['running-versions']
export const runningPath = '/api/v1/versions/running'

/** What every service runs in every environment. The server caches it for two minutes. */
export function useRunningVersions() {
  return useApiGet<RunningMatrix>(runningKey, runningPath)
}
