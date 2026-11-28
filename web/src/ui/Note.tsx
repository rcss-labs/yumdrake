import type { ComponentPropsWithoutRef } from 'react'
import { cx } from './classnames'
import './Note.css'

export interface NoteProps extends ComponentPropsWithoutRef<'div'> {
  tone?: 'info' | 'success' | 'warning' | 'error'
}

// Errors are announced; info and warning are plain content.
export function Note({ tone = 'info', className, ...rest }: NoteProps) {
  return <div className={cx('ui-note', className)} data-tone={tone} role={tone === 'error' ? 'alert' : undefined} {...rest} />
}
