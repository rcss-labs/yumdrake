import { useApiGet } from '../../api/hooks'
import type { QueueView } from '../../api/types'

export const queuePath = '/api/v1/queue'
export const queueRefreshMs = 5_000

/** The queue, asked for again every 5 seconds. */
export function useQueue() {
  return useApiGet<QueueView>(['queue'], queuePath, { refetchInterval: queueRefreshMs })
}
