import type { AccountBalance, BalanceSource } from './types'

/** Keep arbitrary decimal precision: formatting never coerces money to Number. */
export function decimalAmount(raw: string): string {
  const match = /^(-?)(\d+)(?:\.(\d+))?$/.exec(raw.trim())
  if (!match) return '—'
  const integer = match[2].replace(/^0+(?=\d)/, '')
  const fraction = (match[3] ?? '').replace(/0+$/, '')
  const sign = match[1] && (integer !== '0' || /[1-9]/.test(fraction)) ? '-' : ''
  return `${sign}${integer.replace(/\B(?=(\d{3})+(?!\d))/g, ',')}.${fraction.padEnd(2, '0')}`
}

/** Legacy table precision, rounded as decimal text; the stored value stays exact. */
export function compactAmount(raw: string): string {
  const match = /^(-?)(\d+)(?:\.(\d+))?$/.exec(raw.trim())
  if (!match) return '—'
  const integer = match[2].replace(/^0+(?=\d)/, '')
  const fraction = match[3] ?? ''
  const places = integer.length >= 3 ? 0 : 2
  const digits = integer + fraction.slice(0, places).padEnd(places, '0')
  const rounded = BigInt(digits) + (Number(fraction[places] ?? '0') >= 5 ? 1n : 0n)
  const value = rounded.toString().padStart(places + 1, '0')
  const sign = match[1] && rounded !== 0n ? '-' : ''
  return sign + (places ? `${value.slice(0, -places)}.${value.slice(-places)}` : value)
}

/** Compare the original decimal before rounding: exactly one million is finite. */
export function unlimitedAmount(raw: string): boolean {
  const match = /^(\d+)(?:\.(\d+))?$/.exec(raw.trim())
  if (!match) return false
  const integer = match[1].replace(/^0+(?=\d)/, '')
  return integer.length > 7 || (integer.length === 7 &&
    (integer > '1000000' || (integer === '1000000' && /[1-9]/.test(match[2] ?? ''))))
}

export function visibleBalance(summary?: AccountBalance): AccountBalance['balance'] {
  if (!summary || ['unbound', 'unsupported', 'not_found', 'rebind_required'].includes(summary.status)) return null
  return summary.balance
}

export function sourceVerified(source: BalanceSource | undefined): boolean {
  return Boolean(source && ['ok', 'low'].includes(source.status) && source.account_id > 0 &&
    source.last_balance?.account_id === source.account_id && source.last_balance.unit.trim() &&
    decimalAmount(source.last_balance.remaining) !== '—')
}

export function balanceErrorKey(error: unknown): string {
  const value = error as { error?: string; reason?: string; code?: string; status?: number } | null
  const code = value?.error || value?.reason || value?.code || ''
  const known: Record<string, string> = {
    wallet_source_linked: 'sourceLinked', source_has_account_bindings: 'sourceLinked', wallet_source_delete_reserved: 'sourceDeleting',
    wallet_settings_changed: 'conflict', wallet_member_linked: 'walletMemberLinked', wallet_rebind_required: 'rebind', wallet_settings_invalid: 'walletSettingsInvalid', wallet_bridge_unavailable: 'walletBridgeUnavailable', wallet_recipients_required: 'walletRecipientsRequired', wallet_shared_confirmation_required: 'walletSharedConfirmation', wallet_representative_required: 'walletRepresentativeRequired', wallet_unit_changed: 'thresholdUnitMismatch',
    wallet_poll_timeout: 'queryStillRunning', upstream_timeout: 'queryTimeout', ECONNABORTED: 'queryStillRunning', ETIMEDOUT: 'queryStillRunning',
    wallet_snapshot_unavailable: 'snapshotUnavailable',
    upstream_credential_invalid: 'invalidPAT', upstream_user_id_required: 'userIDRequired', upstream_unavailable: 'upstreamUnavailable',
    upstream_response_invalid: 'invalidResponse', upstream_unit_unconfirmed: 'unitUnconfirmed',
    upstream_account_changed: 'upstreamAccountChanged', alert_unit_differs_from_upstream: 'thresholdUnitMismatch',
    account_already_monitored: 'sourceExists', source_changed_refresh_page: 'conflict',
    binding_changed_refresh_page: 'conflict', refresh_in_progress_or_rate_limited: 'rateLimited',
    https_site_origin_required: 'invalidOrigin', public_site_required: 'invalidOrigin',
    invalid_alert_threshold_or_unit: 'invalidThreshold', invalid_name_or_user_id: 'invalidName',
    site_or_account_is_immutable_create_another_source: 'immutableOrigin',
    source_not_verified: 'notVerified', account_identity_changed: 'rebind',
    source_requires_successful_identity_check: 'notVerified', account_binding_changed: 'conflict',
    wallet_config_changed: 'conflict', wallet_url_invalid: 'invalidOrigin', invalid_base_url: 'invalidOrigin',
    wallet_not_verified: 'walletNotVerified', wallet_unsupported: 'walletUnsupported'
  }
  if (known[code]) return `sourcesBalance.errors.${known[code]}`
  if (value?.status === 409) return 'sourcesBalance.errors.conflict'
  if (value?.status === 401) return 'sourcesBalance.errors.login'
  if (value?.status === 403) return 'sourcesBalance.errors.admin'
  if (value?.status === 429) return 'sourcesBalance.errors.rateLimited'
  return 'sourcesBalance.errors.unavailable'
}

export function normalizeWalletURL(raw: string): string | null {
  try {
    const value = raw.trim()
    const url = new URL(value)
    if (!/^https:\/\//i.test(value) || url.protocol !== 'https:' || url.username || url.password ||
      url.search || url.hash || value.includes('?') || value.includes('#') || value.includes('\\') ||
      !['', '/', '/v1', '/v1/'].includes(url.pathname) || value.length > 2048) return null
    return url.origin
  } catch { return null }
}

export function storedUserID(raw: string | null): number | null {
  if (!raw) return null
  try {
    const value: unknown = JSON.parse(raw)
    const id = value && typeof value === 'object' && 'id' in value ? value.id : null
    return typeof id === 'number' && Number.isSafeInteger(id) && id > 0 ? id : null
  } catch { return null }
}

export function identityChanged(event: StorageEvent, owner?: number | null): boolean {
  if (event.key === null || (event.key === 'auth_token' && !event.newValue)) return true
  if (event.key !== 'auth_user') return false
  const before = storedUserID(event.oldValue), after = storedUserID(event.newValue)
  return before === null || after === null || before !== after || (owner != null && after !== owner)
}
