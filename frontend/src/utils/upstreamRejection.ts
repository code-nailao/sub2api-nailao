export const CONTENT_REJECTION_PROFILE = 'grok_content_rejection_v1'

export function isValidRejectionAmount(value: unknown, enabled: boolean): value is number {
  if (typeof value !== 'number' || !Number.isFinite(value) || value < 0 || value >= 1e12 || (enabled && value <= 0)) return false
  return Number(value.toFixed(8)) === value
}

export function isUnprefixedGrokModel(model: string): boolean {
  const id = model.trim()
  return (id === 'grok' || id.startsWith('grok-')) && !id.includes('/') && !id.includes('*')
}
