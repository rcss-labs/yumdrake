import { useQueryClient } from '@tanstack/react-query'
import { Fragment, useId, useState } from 'react'
import { Link, useSearchParams } from 'react-router-dom'
import { api } from '../../api/client'
import { useCan } from '../../api/hooks'
import type { EnvironmentSummary, RunningCell, RunningMatrix, RunningRow } from '../../api/types'
import { Button, Card, Checkbox, ErrorState, Loading, PageHeader, SearchBox, Tag, relativeTime, Note } from '../../ui'
import { servicePath } from '../service/servicePath'
import { Marker, MarkerWithWords, markersOf, type MarkerKey } from './markers'
import { runningKey, runningPath, useRunningVersions } from './queries'
import './versions.css'

const markerOrder: MarkerKey[] = ['behind', 'failed', 'outside']

/** A row is worth a look when something is marked, or two environments with the same label run different versions. */
function differs(row: RunningRow, environments: EnvironmentSummary[]): boolean {
  const seen = new Map<string, Set<string>>()
  for (const [index, cell] of row.cells.entries()) {
    if (markersOf(cell).length > 0) return true
    if (cell.state !== 'ok') continue
    const label = environments[index]?.label ?? ''
    seen.set(label, (seen.get(label) ?? new Set<string>()).add(cell.version))
  }
  return Array.from(seen.values()).some((versions) => versions.size > 1)
}

function CellView({ cell }: { cell: RunningCell }) {
  if (cell.state === 'absent') return <span className="versions-cell versions-muted">not set up</span>
  if (cell.state === 'error') {
    return (
      <span className="versions-cell versions-error">
        could not read
        <span className="mono">{cell.error}</span>
      </span>
    )
  }
  return (
    <span className="versions-cell" title={`${cell.task_definition} · read ${relativeTime(cell.read_at)}`}>
      {cell.version === '' ? <span className="versions-muted">unknown version</span> : <span className="mono">{cell.version}</span>}
      {markersOf(cell).map((marker) => (
        <Marker key={marker} marker={marker} />
      ))}
    </span>
  )
}

function DetailLines({ cell }: { cell: RunningCell }) {
  if (cell.state === 'absent') return <span className="versions-detail-line">Not set up</span>
  if (cell.state === 'error') return <span className="versions-detail-line">Could not read: {cell.error}</span>
  const markers = markersOf(cell)
  return (
    <>
      <span className="versions-detail-line">
        {cell.version === '' ? 'Unknown version' : <span className="mono">{cell.version}</span>}
      </span>
      <span className="versions-detail-line mono">{cell.task_definition}</span>
      <span className="versions-detail-line">read {relativeTime(cell.read_at)}</span>
      {cell.by_yumdrake && cell.run_id > 0 && (
        <Link className="versions-detail-line" to={`/runs/${cell.run_id}`}>
          #{cell.run_id}
        </Link>
      )}
      {markers.map((marker) => (
        <MarkerWithWords key={marker} marker={marker} />
      ))}
    </>
  )
}

function Chevron() {
  return (
    <svg viewBox="0 0 16 16" aria-hidden="true" focusable="false">
      <path d="m4 6 4 4 4-4" />
    </svg>
  )
}

function MatrixTable({ matrix, rows }: { matrix: RunningMatrix; rows: RunningRow[] }) {
  const [open, setOpen] = useState<ReadonlySet<string>>(new Set())
  const prefix = useId()
  const columns = matrix.environments.length + 1

  function toggle(service: string) {
    const next = new Set(open)
    if (!next.delete(service)) next.add(service)
    setOpen(next)
  }

  return (
    <div className="versions-scroll">
      <table className="ui-table versions-table" aria-label="Running versions">
        <thead>
          <tr>
            <th scope="col" className="versions-service-col">
              Service
            </th>
            {matrix.environments.map((environment) => (
              <th key={environment.name} scope="col">
                <span className="versions-head">
                  <span className="mono">{environment.name}</span>
                  <Tag>{environment.label}</Tag>
                </span>
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => {
            const expanded = open.has(row.service)
            const detailId = `${prefix}-${row.service}`
            return (
              <Fragment key={row.service}>
                <tr>
                  <td className="versions-service-col">
                    <div className="versions-svc">
                      <Link className="mono" to={servicePath(row.service)}>
                        {row.service}
                      </Link>
                      <button
                        type="button"
                        className="versions-toggle"
                        aria-expanded={expanded}
                        aria-controls={expanded ? detailId : undefined}
                        aria-label={`Show details for ${row.service}`}
                        onClick={() => toggle(row.service)}
                      >
                        <Chevron />
                      </button>
                    </div>
                  </td>
                  {row.cells.map((cell, index) => (
                    <td key={matrix.environments[index]?.name ?? index}>
                      <CellView cell={cell} />
                    </td>
                  ))}
                </tr>
                {expanded && (
                  <tr>
                    <td colSpan={columns} className="versions-detail-cell">
                      <section id={detailId} className="versions-detail" aria-label={`${row.service} in each environment`}>
                        <ul>
                          {row.cells.map((cell, index) => (
                            <li key={matrix.environments[index]?.name ?? index}>
                              <span className="versions-detail-env">
                                <span className="mono">{matrix.environments[index]?.name}</span>
                                <Tag>{matrix.environments[index]?.label}</Tag>
                              </span>
                              <DetailLines cell={cell} />
                            </li>
                          ))}
                        </ul>
                      </section>
                    </td>
                  </tr>
                )}
              </Fragment>
            )
          })}
        </tbody>
      </table>
    </div>
  )
}

function ReadAgain() {
  const queryClient = useQueryClient()
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState('')

  async function readAgain() {
    setBusy(true)
    setFailure('')
    try {
      queryClient.setQueryData(runningKey, await api<RunningMatrix>('GET', `${runningPath}?refresh=1`))
    } catch (caught) {
      setFailure(caught instanceof Error ? caught.message : 'The versions could not be read again. Try again.')
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <Button loading={busy} onClick={() => void readAgain()}>
        Read again
      </Button>
      {failure !== '' && <Note tone="error">{failure}</Note>}
    </>
  )
}

export function VersionsPage() {
  const [params, setParams] = useSearchParams()
  const can = useCan()
  const query = useRunningVersions()
  const search = params.get('q') ?? ''
  const onlyDifferences = params.get('diff') === '1'

  function change(name: 'q' | 'diff', value: string) {
    const next = new URLSearchParams(params)
    if (value === '') next.delete(name)
    else next.set(name, value)
    setParams(next, { replace: true })
  }

  const header = (
    <PageHeader
      title="Versions"
      lead="What every service is running in every environment, read from ECS."
      actions={can('manage_connections') ? <ReadAgain /> : undefined}
    />
  )
  if (query.isPending) {
    return (
      <>
        {header}
        <Loading />
      </>
    )
  }
  if (query.isError) {
    return (
      <>
        {header}
        <ErrorState error={query.error} onRetry={() => void query.refetch()} />
      </>
    )
  }

  const matrix = query.data
  const needle = search.trim().toLowerCase()
  const rows = matrix.rows.filter(
    (row) => row.service.toLowerCase().includes(needle) && (!onlyDifferences || differs(row, matrix.environments)),
  )
  const empty =
    matrix.rows.length === 0
      ? 'No services in the inventory'
      : needle !== '' && !matrix.rows.some((row) => row.service.toLowerCase().includes(needle))
        ? `No service matches “${search}”`
        : 'No service differs between environments with the same label'

  return (
    <>
      {header}
      <div className="versions-toolbar">
        <SearchBox label="Find a service" value={search} onChange={(value) => change('q', value)} />
        <Checkbox label="Only differences" checked={onlyDifferences} onChange={(event) => change('diff', event.target.checked ? '1' : '')} />
        <ul className="versions-legend" aria-label="Markers">
          {markerOrder.map((marker) => (
            <li key={marker}>
              <MarkerWithWords marker={marker} />
            </li>
          ))}
        </ul>
      </div>
      <Card flush>
        {rows.length === 0 ? <p className="versions-empty">{empty}</p> : <MatrixTable matrix={matrix} rows={rows} />}
      </Card>
      <p className="versions-count">
        {rows.length} of {matrix.rows.length} services
      </p>
    </>
  )
}
