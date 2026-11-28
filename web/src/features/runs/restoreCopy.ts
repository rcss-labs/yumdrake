import { plural } from './runModel'

/** The words of the restore confirmation, shared by the run page and the service page. */
export function restoreDialogCopy(services: string[]): { title: string; body: string; confirmLabel: string } {
  return {
    title: `Restore ${plural(services.length, 'service')}?`,
    body: `This plans a restore of ${services.join(', ')} to what they ran before. Nothing changes until you start the restore run.`,
    confirmLabel: 'Plan the restore',
  }
}
