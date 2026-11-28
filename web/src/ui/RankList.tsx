import { Link } from 'react-router-dom'
import './RankList.css'

export interface RankItem {
  key: string
  label: string
  value: number
  detail?: string
  href?: string
}

export interface RankListProps {
  items: RankItem[]
  /** The value as words, for example "3 failures". */
  valueLabel: (value: number) => string
  /** Shown when there are no items. */
  empty: string
}

export function RankList({ items, valueLabel, empty }: RankListProps) {
  if (items.length === 0) return <p className="ui-rank-empty">{empty}</p>
  const largest = Math.max(...items.map((item) => item.value), 1)
  return (
    <ol className="ui-rank">
      {items.map((item) => (
        <li key={item.key} className="ui-rank-item">
          <div className="ui-rank-top">
            {item.href ? (
              <Link to={item.href} className="mono ui-rank-label">
                {item.label}
              </Link>
            ) : (
              <span className="mono ui-rank-label">{item.label}</span>
            )}
            <span className="num ui-rank-value">{valueLabel(item.value)}</span>
          </div>
          <div className="ui-rank-bar" aria-hidden="true">
            <span style={{ width: `${(item.value / largest) * 100}%` }} />
          </div>
          {item.detail && <div className="ui-rank-detail">{item.detail}</div>}
        </li>
      ))}
    </ol>
  )
}
