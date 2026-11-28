import { Route, Routes } from 'react-router-dom'
import { AdminPage } from './features/admin/AdminPage'
import { ApprovalsPage } from './features/approvals/ApprovalsPage'
import { InventoryPage } from './features/inventory/InventoryPage'
import { OverviewPage } from './features/overview/OverviewPage'
import { JobPage } from './features/jobs/JobPage'
import { JobsPage } from './features/jobs/JobsPage'
import { QueuePage } from './features/queue/QueuePage'
import { RunPage } from './features/runs/RunPage'
import { RunsPage } from './features/runs/RunsPage'
import { ConfirmDevicePage } from './features/tokens/ConfirmDevicePage'
import { TokensPage } from './features/tokens/TokensPage'
import { ServicePage } from './features/service/ServicePage'
import { VersionsPage } from './features/versions/VersionsPage'
import { NotFound } from './shell/NotFound'

/** The whole route table. The sign-in address is handled by the session provider, not here. */
export function AppRoutes() {
  return (
    <Routes>
      <Route path="/" element={<OverviewPage />} />
      <Route path="/jobs" element={<JobsPage />} />
      <Route path="/jobs/:id" element={<JobPage />} />
      <Route path="/runs" element={<RunsPage />} />
      <Route path="/runs/:id" element={<RunPage />} />
      <Route path="/queue" element={<QueuePage />} />
      <Route path="/approvals" element={<ApprovalsPage />} />
      <Route path="/versions" element={<VersionsPage />} />
      <Route path="/services/:name" element={<ServicePage />} />
      <Route path="/inventory" element={<InventoryPage />} />
      <Route path="/cli" element={<TokensPage />} />
      <Route path="/cli/confirm" element={<ConfirmDevicePage />} />
      <Route path="/admin/:section?" element={<AdminPage />} />
      <Route path="*" element={<NotFound />} />
    </Routes>
  )
}
