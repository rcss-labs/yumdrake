// The action bar of the run page: only the buttons the person may press and the run's state allows.
import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useCan } from '../../api/hooks'
import { ApiError } from '../../api/error'
import type { RunDetail } from '../../api/types'
import { Button, ConfirmDialog, Note } from '../../ui'
import {
  useApproveRun,
  useCancelRun,
  usePlanAgain,
  useRejectRun,
  useRerunFailed,
  useRestoreRun,
  useStartRun,
} from './actions'
import { failedChecks } from './planModel'
import { useApprovals, useRunPlan } from './queries'
import { restoreDialogCopy } from './restoreCopy'
import { changedServices, selectionToRepeat, serviceProgress } from './runModel'

type Confirming = 'start' | 'cancel' | 'discard' | 'reject' | 'restore'

export interface RunActionsProps {
  run: RunDetail
  /** Fetches the run again; called after an action and after a 409. */
  reload(): void
}

const restorable = ['succeeded', 'partially_succeeded', 'failed', 'cancelled']
const mayRerun = ['partially_succeeded', 'failed', 'cancelled']
const mayPlanAgain = ['invalidated', 'rejected', 'expired']

export function RunActions({ run, reload }: RunActionsProps) {
  const can = useCan()
  const navigate = useNavigate()
  const [confirming, setConfirming] = useState<Confirming | null>(null)
  const [busy, setBusy] = useState(false)
  // A message about an action that did not work. It belongs to the state the run was in; a later change of state
  // (someone else acting, a state event) removes it. After a 409 the reload is expected to change the state once,
  // and the message stays for that state: `settled` records it.
  const [notice, setNotice] = useState<{ text: string; from: string; reloading: boolean; settled?: string } | null>(null)
  if (notice?.reloading && !notice.settled && run.state !== notice.from) setNotice({ ...notice, settled: run.state })
  const noticeText = notice && run.state === (notice.settled ?? notice.from) ? notice.text : ''

  const target = { label: run.label, environment: run.environment, folderId: run.folder_id }
  const mayRun = can('run', target)
  const mayCancel = can('cancel', target)
  const mayRestore = can('restore', target)
  // Whether this viewer may decide the run, and why not, is the /approvals row of this run; no row, no buttons.
  const mayAsk = run.state === 'waiting_approval' && can('approve', target)
  const approvals = useApprovals({ enabled: mayAsk })
  const approvalRow = mayAsk ? approvals.data?.approvals.find((item) => item.run_id === run.id) : undefined
  const mayApprove = approvalRow?.can_approve === true

  // Start waits for the plan: it is hidden until the plan has loaded and while any of its checks failed.
  // Approve and Reject wait for it too (they stay disabled), and Approve names the plan the person was shown.
  const plan = useRunPlan(run.id, { enabled: (run.state === 'planned' && mayRun) || (run.state === 'waiting_approval' && mayApprove) })
  const planReady = plan.isSuccess && failedChecks(plan.data).length === 0
  const planFailed = (run.state === 'planned' && mayRun && plan.isError) || (run.state === 'waiting_approval' && mayApprove && plan.isError)

  const start = useStartRun(run.id)
  const cancel = useCancelRun(run.id)
  const approve = useApproveRun(run.id)
  const reject = useRejectRun(run.id)
  const rerun = useRerunFailed(run.id)
  const restore = useRestoreRun(run.id)
  const planAgain = usePlanAgain(run.job_source_id)

  // Runs an action. A 409 means the run changed meanwhile: reload it and say what the server said.
  const perform = async (action: () => Promise<RunDetail>, goToResult = false) => {
    setNotice(null)
    setBusy(true)
    try {
      const result = await action()
      if (goToResult) navigate(`/runs/${result.id}`)
      else reload()
    } catch (caught) {
      const conflict = caught instanceof ApiError && caught.status === 409
      if (conflict) reload()
      setNotice({
        text: caught instanceof Error ? caught.message : 'The action did not work. Try again.',
        from: run.state,
        reloading: conflict,
      })
    } finally {
      setBusy(false)
    }
  }

  const confirmThen = (action: () => Promise<RunDetail>, goToResult = false) => {
    setConfirming(null)
    void perform(action, goToResult)
  }

  const startNow = () => perform(() => start.mutateAsync({ manifest_id: run.manifest_id }))
  const askOrStart = () => (run.label === 'prod' ? setConfirming('start') : void startNow())

  const buttons: { name: string; variant?: 'primary' | 'danger'; disabled?: boolean; onClick(): void }[] = []
  const state = run.state
  if (state === 'planned') {
    if (mayRun && planReady) buttons.push({ name: 'Start', variant: 'primary', onClick: askOrStart })
    if (mayCancel) buttons.push({ name: 'Discard', onClick: () => setConfirming('discard') })
  }
  if (state === 'waiting_approval' && mayApprove) {
    const shownPlan = plan.data
    buttons.push({
      name: 'Approve',
      variant: 'primary',
      disabled: !shownPlan,
      onClick: () => shownPlan && void perform(() => approve.mutateAsync({ manifest_id: shownPlan.id })),
    })
    buttons.push({ name: 'Reject', variant: 'danger', disabled: !shownPlan, onClick: () => setConfirming('reject') })
  }
  if ((state === 'waiting_approval' || state === 'queued' || state === 'running') && mayCancel) {
    buttons.push({ name: 'Cancel', onClick: () => setConfirming('cancel') })
  }
  const progress = serviceProgress(run)
  const changed = changedServices(run)
  if (mayRerun.includes(state) && mayRun && progress.some((service) => !service.updated)) {
    buttons.push({ name: 'Re-run failed', onClick: () => void perform(() => rerun.mutateAsync({}), true) })
  }
  if (restorable.includes(state) && mayRestore && changed.length > 0) {
    buttons.push({ name: 'Restore', onClick: () => setConfirming('restore') })
  }
  if (mayPlanAgain.includes(state) && mayRun) {
    buttons.push({
      name: 'Plan again',
      onClick: () =>
        void perform(
          () => planAgain.mutateAsync({ environment: run.environment, selection: selectionToRepeat(run), start: false }),
          true,
        ),
    })
  }

  const approvalReason = approvalRow && !approvalRow.can_approve ? approvalRow.reason : ''

  return (
    <div className="run-actions-area">
      {buttons.length > 0 && (
        <div className="run-actions" role="group" aria-label="Run actions">
          {buttons.map((button) => (
            <Button key={button.name} variant={button.variant} loading={busy} disabled={button.disabled} onClick={button.onClick}>
              {button.name}
            </Button>
          ))}
        </div>
      )}
      {approvalReason && <Note>{approvalReason}</Note>}
      {planFailed && (
        <Note tone="error">
          {run.state === 'planned' ? 'Start is hidden until the plan loads.' : 'Approve and Reject wait for the plan to load.'}{' '}
          <Button size="sm" onClick={() => void plan.refetch()}>
            Try again
          </Button>
        </Note>
      )}
      {noticeText && <Note tone="error">{noticeText}</Note>}

      <ConfirmDialog
        open={confirming === 'start'}
        title={`Start in ${run.environment}?`}
        body={`Services in ${run.environment} will start updating now. You can cancel the run, but nothing is rolled back.`}
        confirmLabel="Start in prod"
        danger
        onConfirm={() => confirmThen(() => start.mutateAsync({ manifest_id: run.manifest_id }))}
        onCancel={() => setConfirming(null)}
      />
      <ConfirmDialog
        open={confirming === 'cancel'}
        title="Cancel this run?"
        body="Steps that have not started will not run. Services already rolling will finish. Nothing is rolled back."
        confirmLabel="Cancel run"
        danger
        onConfirm={() => confirmThen(() => cancel.mutateAsync())}
        onCancel={() => setConfirming(null)}
      />
      <ConfirmDialog
        open={confirming === 'discard'}
        title="Discard this plan?"
        body="The plan will be thrown away and nothing will be deployed. You can plan again later."
        confirmLabel="Discard plan"
        danger
        onConfirm={() => confirmThen(() => cancel.mutateAsync())}
        onCancel={() => setConfirming(null)}
      />
      <ConfirmDialog
        open={confirming === 'reject'}
        title="Reject this run?"
        body="The run will not start and it cannot be approved later. Nothing was deployed."
        confirmLabel="Reject run"
        danger
        onConfirm={() => confirmThen(() => reject.mutateAsync({}))}
        onCancel={() => setConfirming(null)}
      />
      <ConfirmDialog
        open={confirming === 'restore'}
        {...restoreDialogCopy(changed)}
        danger
        onConfirm={() => confirmThen(() => restore.mutateAsync({}), true)}
        onCancel={() => setConfirming(null)}
      />
    </div>
  )
}
