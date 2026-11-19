export interface BalanceSnapshot {
  account_id: number
  remaining: string
  used: string
  unit: string
  checked_at: string
}

export type AccountBalanceStatus = 'unbound' | 'ready' | 'low' | 'pending' | 'error' |
  'rebind_required' | 'unsupported' | 'not_found' | 'unit_unconfirmed' | 'wallet_unavailable' | 'automatic_unavailable'

export interface AccountBalance {
  wallet_id?: string
  wallet_name?: string
  provider?: 'sub' | 'new'
  representative_id?: number
  querying?: boolean
  account_id: number
  automatic?: boolean
  auto_refresh?: boolean
  next_check_at?: string
  last_attempt_at?: string
  query_url?: string
  supported: boolean
  status: AccountBalanceStatus
  source_id?: string
  source_name?: string
  binding_revision: number
  shared_count: number
  balance: BalanceSnapshot | null
  last_checked_at: string | null
  stale: boolean
  error?: string
}

export interface BalanceSource {
  querying?: boolean
  id: string
  name: string
  origin: string
  account_id: number
  threshold: string
  threshold_unit: string
  last_balance?: BalanceSnapshot | null
  last_checked_at: string
  next_check_at: string
  status: string
  error?: string
  alert_pending: boolean
  updated_by: number
  revision: number
}

export interface SourceInput {
  name: string
  origin: string
  account_id: number
  personal_token: string
  threshold: string
  threshold_unit: string
  revision: number
}

export interface BalanceAccount { id: number; name: string; type: string; platform: string }

export interface WalletConfiguration {
  url: string
  call_url: string
  revision: string
  supported: boolean
}

export interface WalletValue {
  wallet_id?: string
  wallet_name?: string
  provider?: 'sub' | 'new'
  source_id?: string
  representative_id?: number
  shared_count?: number
  querying?: boolean
  auto_refresh?: boolean
  next_check_at?: string
  last_attempt_at?: string
  status: string
  remaining?: string
  unit?: string
  checked_at?: string
  query_url?: string
  stale: boolean
  error?: string
}

export interface WalletProbe {
  url: string
  wallet: WalletValue
  verification_token?: string
}

export interface BalanceAPI {
  walletConfiguration(accountID: number, signal?: AbortSignal): Promise<WalletConfiguration>
  testWallet(accountID: number, url: string, revision: string, signal?: AbortSignal): Promise<WalletProbe>
  saveWallet(accountID: number, url: string, revision: string, verificationToken?: string, signal?: AbortSignal): Promise<WalletConfiguration>
  automatic(accountID: number, refresh?: boolean, signal?: AbortSignal): Promise<AccountBalance>
  summaries(ids: number[], signal?: AbortSignal): Promise<AccountBalance[]>
  sources(signal?: AbortSignal): Promise<BalanceSource[]>
  saveSource(id: string | null, input: SourceInput): Promise<BalanceSource>
  refreshSource(id: string, signal?: AbortSignal): Promise<BalanceSource>
  bind(accountID: number, source: BalanceSource, revision: number): Promise<AccountBalance>
  unbind(accountID: number, revision: number): Promise<AccountBalance>
  refreshAccounts(ids: number[], signal?: AbortSignal): Promise<AccountBalance[]>
}


export interface WalletAlertRule {
  enabled: boolean
  threshold: string
  unit: string
  use_default_recipients: boolean
  recipients: string[]
  repeat_hours: number
  notify_recovery: boolean
}
export interface ManagedWallet {
  id: string
  revision: number
  name: string
  provider: 'sub' | 'new'
  representative_id: number
  source_id?: string
  alert: WalletAlertRule
}
export interface WalletCandidate {
  platform: string
  id: number
  name: string
  site: string
  wallet_id?: string
  verified: boolean
  needs_confirmation: boolean
}
export interface WalletDelivery {
  id: string
  kind: 'low' | 'recovery'
  recipient: string
  status: string
  attempts: number
  created_at: string
  updated_at: string
  error?: string
}
export interface WalletSettingsView {
  wallet: ManagedWallet
  balance: WalletValue
  members: WalletCandidate[]
  candidates: WalletCandidate[]
  wallets: ManagedWallet[]
  default_recipients: string[]
  deliveries: WalletDelivery[]
  bridge_ready: boolean
}
export interface WalletSettingsInput extends ManagedWallet {
  members: number[]
  confirm_shared: boolean
}
