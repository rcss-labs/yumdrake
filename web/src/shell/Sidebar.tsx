import { useState } from 'react'
import { NavLink } from 'react-router-dom'
import { errorMessage } from '../api/client'
import type { Me } from '../api/types'
import { canSomewhere } from '../auth/permissions'
import { useMe, useSignOut } from '../auth/session'
import { useApprovals } from '../features/runs/queries'
import { Icon, type IconName } from '../ui'

interface NavItem {
  to: string
  label: string
  icon: IconName
}

// Groups are separated by a gap, as in the mockup.
const navGroups: NavItem[][] = [
  [
    { to: '/', label: 'Overview', icon: 'overview' },
    { to: '/jobs', label: 'Jobs', icon: 'jobs' },
    { to: '/runs', label: 'Runs', icon: 'runs' },
    { to: '/queue', label: 'Queue', icon: 'queue' },
    { to: '/approvals', label: 'Approvals', icon: 'approvals' },
  ],
  [
    { to: '/versions', label: 'Versions', icon: 'versions' },
    { to: '/inventory', label: 'Inventory', icon: 'inventory' },
  ],
  [{ to: '/cli', label: 'CLI', icon: 'cli' }],
]

const adminItem: NavItem = { to: '/admin', label: 'Admin', icon: 'admin' }

function seesAdmin(me: Me): boolean {
  return (
    me.admin ||
    canSomewhere(me, 'manage_connections') ||
    canSomewhere(me, 'manage_access') ||
    canSomewhere(me, 'view_audit')
  )
}

const approvalsRefreshMs = 30_000

/** How many waiting runs this person may approve; 0 while loading or when the request fails. */
function useApprovalCount(): number {
  const approvals = useApprovals({ refetchInterval: approvalsRefreshMs })
  return approvals.data?.approvals.filter((item) => item.can_approve).length ?? 0
}

function SidebarLink({ item, count = 0 }: { item: NavItem; count?: number }) {
  return (
    <NavLink
      to={item.to}
      end={item.to === '/'}
      className="shell-nav"
      aria-label={count > 0 ? `${item.label}, ${count} waiting` : undefined}
    >
      <Icon name={item.icon} />
      <span>{item.label}</span>
      {count > 0 && (
        <span className="shell-count" aria-hidden="true">
          {count}
        </span>
      )}
    </NavLink>
  )
}

export function Sidebar() {
  const me = useMe()
  const signOut = useSignOut()
  const approvalCount = useApprovalCount()
  const [signingOut, setSigningOut] = useState(false)
  const [signOutError, setSignOutError] = useState('')

  async function onSignOut() {
    setSigningOut(true)
    setSignOutError('')
    try {
      await signOut()
    } catch (error) {
      setSignOutError(`We could not sign you out. ${errorMessage(error)}`)
      setSigningOut(false)
    }
  }

  const groups = navGroups.map((group) => [...group])
  if (seesAdmin(me)) {
    groups[groups.length - 1]?.push(adminItem)
  }

  return (
    <div className="shell-side">
      <div className="shell-brand">
        <span className="shell-logo" aria-hidden="true">
          <span style={{ height: 10 }} />
          <span style={{ height: 16 }} />
          <span style={{ height: 6 }} />
        </span>
        Yumdrake
      </div>
      <nav aria-label="Main" className="shell-navlist">
        {groups.map((group) => (
          <div key={group[0]?.to} className="shell-navgroup">
            {group.map((item) => (
              <SidebarLink key={item.to} item={item} count={item.to === '/approvals' ? approvalCount : 0} />
            ))}
          </div>
        ))}
      </nav>
      <div className="shell-me">
        <div>
          <div className="shell-me-name">{me.name}</div>
          <div className="shell-me-email">{me.email}</div>
        </div>
        <button type="button" className="shell-signout" disabled={signingOut} onClick={() => void onSignOut()}>
          Sign out
        </button>
        {signOutError && (
          <p className="shell-signout-error" role="alert">
            {signOutError}
          </p>
        )}
      </div>
    </div>
  )
}
