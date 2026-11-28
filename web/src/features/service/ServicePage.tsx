import { useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { ApiError } from '../../api/error'
import { useApiGet, useCan } from '../../api/hooks'
import type { ServiceConfigItem, ServiceDeployment, ServiceEnvironmentRunning, ServicePage as ServicePageBody } from '../../api/types'
import { NotFound } from '../../shell/NotFound'
import { Button, Card, ConfirmDialog, DataTable, ErrorState, Facts, Loading, Note, PageHeader, StatusPill, Tag, Time, relativeTime } from '../../ui'
import type { DataColumn } from '../../ui'
import { ServiceInventoryBlock } from '../inventory/ServiceInventoryBlock'
import { useRestoreRun } from '../runs/actions'
import { restoreDialogCopy } from '../runs/restoreCopy'
import { MarkerWithWords, markersOf } from '../versions/markers'
import './service.css'

interface ConfigRow extends ServiceConfigItem {
  run_id: number
  environment: string
}

const runLink = (runId: number) => <Link to={`/runs/${runId}`}>#{runId}</Link>

const deploymentColumns: DataColumn<ServiceDeployment>[] = [
  { key: 'run', header: 'Run', render: (row) => runLink(row.run_id) },
  { key: 'environment', header: 'Environment', render: (row) => <span className="mono">{row.environment}</span> },
  { key: 'version', header: 'Version', render: (row) => <span className="mono">{row.version}</span> },
  { key: 'state', header: 'State', render: (row) => <StatusPill status={row.state} /> },
  { key: 'at', header: 'When', className: 'svc-nowrap', render: (row) => <Time iso={row.at} /> },
  { key: 'by', header: 'By', render: (row) => row.by },
]

const configColumns: DataColumn<ConfigRow>[] = [
  { key: 'run', header: 'Run', render: (row) => runLink(row.run_id) },
  { key: 'environment', header: 'Environment', render: (row) => <span className="mono">{row.environment}</span> },
  {
    key: 'file',
    header: 'File',
    render: (row) => (
      <span className="mono" title={row.destination}>
        {row.name}
      </span>
    ),
  },
  { key: 'checksum', header: 'Checksum', render: (row) => <span className="mono">{row.checksum}</span> },
]

function Restore({ serviceName, runId }: { serviceName: string; runId: number }) {
  const navigate = useNavigate()
  const restore = useRestoreRun(runId)
  const [asking, setAsking] = useState(false)
  const [failure, setFailure] = useState('')

  async function plan() {
    setAsking(false)
    setFailure('')
    try {
      const run = await restore.mutateAsync({ services: [serviceName] })
      navigate(`/runs/${run.id}`)
    } catch (caught) {
      setFailure(caught instanceof Error ? caught.message : 'The restore could not be planned. Try again.')
    }
  }

  return (
    <>
      <Button loading={restore.isPending} onClick={() => setAsking(true)}>
        Restore
      </Button>
      {failure !== '' && <Note tone="error">{failure}</Note>}
      <ConfirmDialog open={asking} {...restoreDialogCopy([serviceName])} danger onConfirm={() => void plan()} onCancel={() => setAsking(false)} />
    </>
  )
}

function EnvironmentCard({ serviceName, item }: { serviceName: string; item: ServiceEnvironmentRunning }) {
  const can = useCan()
  const { cell } = item
  const mayRestore = item.restore_from_run_id !== null && can('restore', { label: item.label, environment: item.environment, folderId: item.restore_from_folder_id ?? undefined })

  return (
    <Card>
      <section aria-label={`Running in ${item.environment}`} className="svc-env">
        <h3 className="svc-env-name">
          <span className="mono">{item.environment}</span>
          <Tag>{item.label}</Tag>
        </h3>
        {cell.state === 'absent' && <p className="svc-muted">Not set up</p>}
        {cell.state === 'error' && (
          <p className="svc-error">
            Could not read <span className="mono">{cell.error}</span>
          </p>
        )}
        {cell.state === 'ok' && (
          <>
            <p className="svc-version">{cell.version === '' ? <span className="svc-muted">Unknown version</span> : <span className="mono">{cell.version}</span>}</p>
            <ul className="svc-markers">
              {markersOf(cell).map((marker) => (
                <li key={marker}>
                  <MarkerWithWords marker={marker} />
                </li>
              ))}
            </ul>
            <p className="svc-line mono">{cell.task_definition}</p>
            <p className="svc-line">read {relativeTime(cell.read_at)}</p>
            {cell.by_yumdrake && cell.run_id > 0 && <p className="svc-line">Deployed by {runLink(cell.run_id)}</p>}
          </>
        )}
        {mayRestore && item.restore_from_run_id !== null && (
          <div className="svc-actions">
            <Restore serviceName={serviceName} runId={item.restore_from_run_id} />
          </div>
        )}
      </section>
    </Card>
  )
}

export function ServicePage() {
  const { name = '' } = useParams()
  const query = useApiGet<ServicePageBody>(['services', name], `/api/v1/services/${encodeURIComponent(name)}`, { enabled: name !== '' })

  if (query.isPending) return <Loading />
  if (query.isError) {
    if (query.error instanceof ApiError && query.error.status === 404) return <NotFound />
    return <ErrorState error={query.error} onRetry={() => void query.refetch()} />
  }

  const page = query.data
  const { service } = page
  const configRows: ConfigRow[] = page.config.flatMap((change) =>
    change.items.map((item) => ({ ...item, run_id: change.run_id, environment: change.environment })),
  )

  return (
    <>
      <PageHeader title={<span className="mono">{service.name}</span>} crumbs={<Link to="/versions">Versions</Link>} />
      <Facts
        className="svc-facts"
        items={[
          { label: 'Version pattern', value: <span className="mono">{service.versions.pattern}</span> },
          { label: 'Sorted by', value: <span className="mono">{service.versions.sort}</span> },
          { label: 'Versions offered', value: <span className="mono">{service.versions.limit}</span> },
          { label: 'Defined in', value: <span className="mono">{service.file}</span> },
        ]}
      />
      <div className="svc-envs">
        {page.environments.map((item) => (
          <EnvironmentCard key={item.environment} serviceName={service.name} item={item} />
        ))}
      </div>
      <Card flush className="svc-section">
        <h2 className="svc-title">Deployments</h2>
        <DataTable
          label="Deployments"
          columns={deploymentColumns}
          rows={page.deployments}
          rowKey={(row) => row.leg_id}
          minWidth={560}
          empty={<p className="svc-empty">No deployments yet</p>}
        />
      </Card>
      <Card flush className="svc-section">
        <h2 className="svc-title">Config history</h2>
        <DataTable
          label="Config history"
          columns={configColumns}
          rows={configRows}
          rowKey={(row) => `${row.run_id}:${row.environment}:${row.kind}:${row.name}:${row.destination}`}
          minWidth={560}
          empty={<p className="svc-empty">No config changes recorded</p>}
        />
      </Card>
      <section className="svc-section" aria-labelledby="svc-inventory">
        <h2 id="svc-inventory" className="svc-title svc-title-bare">
          Inventory
        </h2>
        <ServiceInventoryBlock service={service} environmentOrder={page.environments.map((item) => item.environment)} withHeading={false} />
      </section>
    </>
  )
}
