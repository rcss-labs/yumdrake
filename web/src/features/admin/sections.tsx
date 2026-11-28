import type { ComponentType } from 'react'
import type { Action } from '../../api/types'
import { AuditSection } from './AuditSection'
import { AWSAccountsSection } from './AWSAccountsSection'
import { FreezesSection } from './FreezesSection'
import { GitHubSection } from './GitHubSection'
import { GroupsSection } from './GroupsSection'
import { HealthPage } from '../health/HealthPage'
import { SlackSection } from './SlackSection'
import { SourcesSection } from './SourcesSection'
import { SSOSection } from './SSOSection'
import { UsersSection } from './UsersSection'

export interface AdminSection {
  /** The last part of the address: /admin/<slug>. */
  slug: string
  label: string
  /** The permission needed to see the section; an administrator sees all. */
  action: Action
  component: ComponentType
}

/** In tab order. Each section component takes no props and makes requests only once it is shown. */
export const adminSections: readonly AdminSection[] = [
  { slug: 'users', label: 'Users', action: 'manage_access', component: UsersSection },
  { slug: 'groups', label: 'Groups and permissions', action: 'manage_access', component: GroupsSection },
  { slug: 'sso', label: 'Single sign-on', action: 'manage_access', component: SSOSection },
  { slug: 'freezes', label: 'Freezes', action: 'manage_access', component: FreezesSection },
  { slug: 'aws', label: 'AWS accounts', action: 'manage_connections', component: AWSAccountsSection },
  { slug: 'github', label: 'GitHub', action: 'manage_connections', component: GitHubSection },
  { slug: 'sources', label: 'Git sources', action: 'manage_connections', component: SourcesSection },
  { slug: 'slack', label: 'Slack', action: 'manage_connections', component: SlackSection },
  { slug: 'health', label: 'System health', action: 'manage_connections', component: HealthPage },
  { slug: 'audit', label: 'Audit log', action: 'view_audit', component: AuditSection },
]
