export function plural(count: number, unit: string): string {
  return `${count} ${unit}${count === 1 ? '' : 's'}`
}

/** "2 minutes" for whole minutes, otherwise "30 seconds". */
export function durationWords(seconds: number): string {
  if (seconds >= 60 && seconds % 60 === 0) return plural(seconds / 60, 'minute')
  return plural(seconds, 'second')
}
