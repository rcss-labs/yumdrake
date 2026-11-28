import { keepPreviousData } from '@tanstack/react-query'
import { useApiGet } from '../../api/hooks'
import type { Overview } from '../../api/types'

/** The overview for one label ("" = every environment) and a period in days. Keeps the old numbers while a new filter loads. */
export function useOverview(label: string, days: number) {
  const query = new URLSearchParams({ days: String(days) })
  if (label !== '') query.set('label', label)
  return useApiGet<Overview>(['analytics', 'overview', label, days], `/api/v1/analytics/overview?${query.toString()}`, {
    placeholderData: keepPreviousData,
  })
}
