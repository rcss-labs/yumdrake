import { Link, useSearchParams } from 'react-router-dom'
import type { Service, ServiceEnvironment } from '../../api/types'
import { CodeBlock, Segmented } from '../../ui'
import { servicePath } from '../service/servicePath'
import './inventory.css'

function EnvironmentBlock({ name, block }: { name: string; block: ServiceEnvironment }) {
  const envFiles = block.env_files ?? []
  const tomls = block.tomls ?? []
  return (
    <section className="inv-block" aria-label={name}>
      <h3 className="mono">{name}</h3>
      <dl className="inv-pairs">
        <div>
          <dt>ECS service</dt>
          <dd className="mono">{block.ecs_service}</dd>
        </div>
        <div>
          <dt>ECR repository</dt>
          <dd className="mono">{block.ecr_repository}</dd>
        </div>
      </dl>
      <FileList title="Env files" emptyText="No env files" files={envFiles} />
      <FileList title="TOML files" emptyText="No TOML files" files={tomls} />
    </section>
  )
}

function FileList({
  title,
  emptyText,
  files,
}: {
  title: string
  emptyText: string
  files: { name: string; source: string; destination: string }[]
}) {
  return (
    <div className="inv-files">
      <h4>{title}</h4>
      {files.length === 0 ? (
        <p className="inv-empty-line">{emptyText}</p>
      ) : (
        <ul>
          {files.map((file) => (
            <li key={`${file.name}:${file.source}`}>
              <span className="mono inv-strong">{file.name}</span>
              <span className="inv-file-line">
                <span className="inv-file-label">Source</span>
                <span className="mono">{file.source}</span>
              </span>
              <span className="inv-file-line">
                <span className="inv-file-label">Destination</span>
                <span className="mono">{file.destination}</span>
              </span>
            </li>
          ))}
        </ul>
      )}
    </div>
  )
}

export interface ServiceInventoryBlockProps {
  service: Service
  /** The environments in the order the inventory lists them; the object's key order is not meaningful. */
  environmentOrder: string[]
  /** Shows the service name (linked to its page) and its version rule above the blocks. The service page has them in its header. */
  withHeading?: boolean
}

/** What the inventory says about one service: its version rule, a block per environment and its file. The environment is `?env=`. */
export function ServiceInventoryBlock({ service, environmentOrder, withHeading = true }: ServiceInventoryBlockProps) {
  const [params, setParams] = useSearchParams()
  const environment = params.get('env') ?? ''
  const present = environmentOrder.filter((env) => env in service.environments)
  const shown = environment !== '' && present.includes(environment) ? [environment] : present
  const options = [{ value: '', label: 'All' }, ...present.map((env) => ({ value: env, label: env }))]

  function pick(value: string) {
    const next = new URLSearchParams(params)
    if (value === '') next.delete('env')
    else next.set('env', value)
    setParams(next)
  }

  return (
    <div className="inv-detail">
      {withHeading && (
        <div>
          <h2 className="mono">
            <Link to={servicePath(service.name)}>{service.name}</Link>
          </h2>
          <dl className="inv-pairs inv-rule">
            <div>
              <dt>Version pattern</dt>
              <dd className="mono">{service.versions.pattern}</dd>
            </div>
            <div>
              <dt>Sorted by</dt>
              <dd className="mono">{service.versions.sort}</dd>
            </div>
            <div>
              <dt>Versions offered</dt>
              <dd className="mono">{service.versions.limit}</dd>
            </div>
          </dl>
        </div>
      )}
      <Segmented label="Environment to show" options={options} value={shown.length === 1 && environment !== '' ? environment : ''} onChange={pick} />
      {shown.map((env) => {
        const block = service.environments[env]
        return block ? <EnvironmentBlock key={env} name={env} block={block} /> : null
      })}
      {service.source !== '' && <CodeBlock label={service.file}>{service.source}</CodeBlock>}
    </div>
  )
}
