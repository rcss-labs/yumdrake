import { useState } from 'react'
import { useSearchParams } from 'react-router-dom'
import type { Inventory } from '../../api/types'
import { Card, ErrorState, Loading, SearchBox } from '../../ui'
import { useInventoryService } from './queries'
import { ServiceInventoryBlock } from './ServiceInventoryBlock'

function ServiceDetail({ name, inventory }: { name: string; inventory: Inventory }) {
  const query = useInventoryService(name)

  if (query.isPending) return <Loading />
  if (query.isError) return <ErrorState error={query.error} onRetry={() => void query.refetch()} />

  // Keep the order of the inventory's environments rather than the object's key order.
  return <ServiceInventoryBlock service={query.data} environmentOrder={inventory.environments.map((env) => env.name)} />
}

export function ServicesTab({ inventory }: { inventory: Inventory }) {
  const [params, setParams] = useSearchParams()
  const [search, setSearch] = useState('')
  const selected = params.get('service') ?? ''
  const total = inventory.environments.length
  const needle = search.trim().toLowerCase()
  const visible = inventory.services.filter((service) => service.name.toLowerCase().includes(needle))

  function choose(name: string) {
    const next = new URLSearchParams(params)
    next.set('service', name)
    next.delete('env')
    setParams(next)
  }

  return (
    <div className="inv-services">
      <Card flush className="inv-list-card">
        <div className="inv-search">
          <SearchBox label="Find a service" value={search} onChange={setSearch} />
        </div>
        {inventory.services.length === 0 ? (
          <p className="inv-empty">No services are defined in this commit.</p>
        ) : visible.length === 0 ? (
          <p className="inv-empty">
            <strong>No service matches</strong>
            <br />
            Clear the search to see every service.
          </p>
        ) : (
          <ul className="inv-list" aria-label="Services">
            {visible.map((service) => (
              <li key={service.name}>
                <button
                  type="button"
                  className="inv-item"
                  aria-current={service.name === selected ? 'true' : undefined}
                  onClick={() => choose(service.name)}
                >
                  <span className="mono">{service.name}</span>
                  <span className="inv-item-sub">
                    {service.environments.length === total ? 'all environments' : `${service.environments.length} of ${total} environments`}
                  </span>
                </button>
              </li>
            ))}
          </ul>
        )}
      </Card>
      <div className="inv-main">
        {selected === '' ? (
          <p className="inv-empty-line">Choose a service to see its environments.</p>
        ) : (
          <ServiceDetail key={selected} name={selected} inventory={inventory} />
        )}
      </div>
    </div>
  )
}
