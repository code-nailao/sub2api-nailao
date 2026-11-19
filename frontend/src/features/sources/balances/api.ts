import { apiClient } from '@/api/client'
import type { AccountBalance, BalanceAPI, BalanceSource, WalletConfiguration, WalletProbe, WalletValue, ManagedWallet, WalletSettingsView, WalletSettingsInput } from './types'

const root = '/api/balance'
// Short requests poll a bounded server operation; no browser request waits six minutes.
const walletRequestTimeout = 20000
const walletPollLimit = 390000

function pause(signal?: AbortSignal): Promise<void> {
  return new Promise((resolve, reject) => {
    const aborted = () => { clearTimeout(timer); reject(new DOMException('Aborted', 'AbortError')) }
    const timer = setTimeout(() => { signal?.removeEventListener('abort', aborted); resolve() }, 2000)
    if (signal?.aborted) aborted()
    else signal?.addEventListener('abort', aborted, { once: true })
  })
}

async function pollWallet<T>(read: (requestID: string) => Promise<T>, pending: (value: T) => boolean, signal?: AbortSignal): Promise<T> {
  const requestID = crypto.randomUUID()
  const deadline = Date.now() + walletPollLimit
  for (;;) {
    if (signal?.aborted) throw new DOMException('Aborted', 'AbortError')
    const result = await read(requestID)
    if (!pending(result)) return result
    if (Date.now() >= deadline) throw { code: 'wallet_poll_timeout' }
    await pause(signal)
  }
}
// Portal shares Main's native authentication and refresh coordination, but its
// routes intentionally sit outside /api/v1. Never forward credentials off-site.
const sameOrigin = { baseURL: '' }

export const balanceAPI: BalanceAPI = {
  async walletConfiguration(accountID, signal) {
    const { data } = await apiClient.get<WalletConfiguration>('/admin/accounts/balance-config', { params: { id: accountID }, signal })
    return data
  },
  async testWallet(accountID, url, revision, signal) {
    return pollWallet(async requestID => {
      const { data } = await apiClient.post<WalletProbe>('/admin/accounts/balance-config/test', { url, revision }, { params: { id: accountID, request_id: requestID }, signal, timeout: walletRequestTimeout })
      return data
    }, value => Boolean(value.wallet.querying), signal)
  },
  async saveWallet(accountID, url, revision, verificationToken, signal) {
    const { data } = await apiClient.put<WalletConfiguration>('/admin/accounts/balance-config', {
      url, revision, ...(verificationToken ? { verification_token: verificationToken } : {})
    }, { params: { id: accountID }, signal, timeout: walletRequestTimeout })
    return data
  },
  async automatic(accountID, refresh = false, signal) {
    const read = async (requestID?: string) => {
      const { data } = await apiClient.get<WalletValue>('/admin/accounts/balance-auto', { params: { id: accountID, ...(refresh ? { refresh: '1', request_id: requestID } : {}) }, signal, timeout: walletRequestTimeout })
      return data
    }
    const data = refresh ? await pollWallet(read, value => Boolean(value.querying), signal) : await read()
    return { account_id: accountID, automatic: true, supported: true, query_url: data.query_url,
      wallet_id: data.wallet_id, wallet_name: data.wallet_name, provider: data.provider, representative_id: data.representative_id, source_id: data.source_id,
      querying: data.querying, auto_refresh: data.auto_refresh, next_check_at: data.next_check_at, last_attempt_at: data.last_attempt_at,
      status: (data.status === 'unsupported' ? 'automatic_unavailable' : data.status) as AccountBalance['status'],
      binding_revision: 0, shared_count: data.shared_count ?? 0, last_checked_at: data.checked_at ?? null, stale: data.stale, error: data.error,
      balance: data.remaining !== undefined && data.unit && data.checked_at
        ? { account_id: 0, remaining: data.remaining, used: '', unit: data.unit, checked_at: data.checked_at } : null }
  },
  async summaries(ids, signal) {
    if (ids.length === 0) return []
    const { data } = await apiClient.get<{ accounts: AccountBalance[] }>(`${root}/accounts`, {
      ...sameOrigin, params: { ids: ids.join(',') }, signal
    })
    return data.accounts
  },
  async sources(signal) {
    const { data } = await apiClient.get<{ sources: BalanceSource[] }>(`${root}/sources`, { ...sameOrigin, signal })
    return data.sources
  },
  async saveSource(id, input) {
    const { data } = id
      ? await apiClient.put<BalanceSource>(`${root}/sources/${encodeURIComponent(id)}`, input, sameOrigin)
      : await apiClient.post<BalanceSource>(`${root}/sources`, input, sameOrigin)
    return data
  },
  async refreshSource(id, signal) {
    return pollWallet(async requestID => {
      const { data } = await apiClient.post<BalanceSource>(`${root}/sources/${encodeURIComponent(id)}/refresh`, undefined, { ...sameOrigin, params: { request_id: requestID }, signal, timeout: walletRequestTimeout })
      return data
    }, value => Boolean(value.querying), signal)
  },
  async bind(accountID, source, revision) {
    const { data } = await apiClient.put<AccountBalance>(`${root}/accounts/${accountID}/binding`, {
      source_id: source.id, source_revision: source.revision, binding_revision: revision
    }, sameOrigin)
    return data
  },
  async unbind(accountID, revision) {
    const { data } = await apiClient.delete<AccountBalance>(`${root}/accounts/${accountID}/binding`, {
      ...sameOrigin, data: { binding_revision: revision }
    })
    return data
  },
  async refreshAccounts(ids, signal) {
    const initial = await balanceAPI.summaries(ids, signal)
    const pending = [...new Set(initial.filter(row => row.supported && row.source_id && row.status !== 'rebind_required').map(row => row.source_id!))]
    const errors = new Map<string, string>()
    await Promise.all(Array.from({ length: Math.min(20, pending.length) }, async () => {
      while (pending.length && !signal?.aborted) {
        const id = pending.shift()!
        try { await balanceAPI.refreshSource(id, signal) } catch (cause) {
          const failure = cause as { error?: string; code?: string }
          errors.set(id, failure?.error || failure?.code || 'upstream_unavailable')
        }
      }
    }))
    const rows = await balanceAPI.summaries(ids, signal)
    return rows.map(row => errors.has(row.source_id ?? '') && row.status !== 'rebind_required'
      ? { ...row, error: errors.get(row.source_id!), stale: Boolean(row.balance) } : row)
  }
}


export const walletManagementAPI = {
  async get(accountID: number, signal?: AbortSignal): Promise<WalletSettingsView> {
    const { data } = await apiClient.get<WalletSettingsView>('/admin/accounts/wallet-settings', { params: { id: accountID }, signal })
    return data
  },
  async save(accountID: number, input: WalletSettingsInput, signal?: AbortSignal): Promise<ManagedWallet> {
    const { data } = await apiClient.put<ManagedWallet>('/admin/accounts/wallet-settings', input, { params: { id: accountID }, signal })
    return data
  },
  async unlink(accountID: number, wallet: ManagedWallet, signal?: AbortSignal) {
    await apiClient.delete('/admin/accounts/wallet-settings', { params: { id: accountID }, data: { id: wallet.id, revision: wallet.revision }, signal })
  },
  async verify(accountID: number, signal?: AbortSignal): Promise<WalletValue> {
    return pollWallet(async requestID => {
      const { data } = await apiClient.post<WalletValue>('/admin/accounts/wallet-settings/verify-channel', undefined, { params: { id: accountID, request_id: requestID }, signal, timeout: walletRequestTimeout })
      return data
    }, value => Boolean(value.querying), signal)
  },
  async preview(accountID: number, input: { name: string; alert: ManagedWallet['alert']; amount: string; names: string[] }, signal?: AbortSignal): Promise<{ subject: string; html: string }> {
    const { data } = await apiClient.post<{ subject: string; html: string }>('/admin/accounts/wallet-settings/preview', input, { params: { id: accountID }, signal })
    return data
  }
}
