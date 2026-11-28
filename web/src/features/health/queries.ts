import { useApiGet } from '../../api/hooks'
import type { SystemHealth } from '../../api/types'

export const healthPath = '/api/v1/system/health'
export const healthRefreshMs = 15_000

/** The system health, asked for again every 15 seconds. */
export function useSystemHealth() {
  return useApiGet<SystemHealth>(['system-health'], healthPath, { refetchInterval: healthRefreshMs })
}
