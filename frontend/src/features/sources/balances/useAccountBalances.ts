import { computed, onScopeDispose, ref, watch, type Ref } from 'vue'
import { balanceAPI } from './api'
import { balanceErrorKey, identityChanged } from './presentation'
import type { AccountBalance, BalanceAccount, BalanceAPI } from './types'

export function useAccountBalances(
  accounts: Ref<BalanceAccount[]>, owner: () => number | null | undefined, api: BalanceAPI = balanceAPI
) {
  const summaries = ref<Record<number, AccountBalance>>({})
  const loading = ref(false)
  const unavailable = ref(false)
  const refreshing = ref(false)
  const refreshFailed = ref(false)
  let version = 0
  let disposed = false
  let sessionInvalidated = false
  let controller: AbortController | undefined
  let refreshController: AbortController | undefined
  const currentOwner = () => owner() ?? null
  const supported = (account: BalanceAccount) => account.type === 'apikey' && account.platform !== 'antigravity'
  const supportsRefresh = computed(() => !loading.value && accounts.value.some(account => {
    const summary = summaries.value[account.id]
    return supported(account) && (!summary?.source_id || summary.status !== 'rebind_required')
  }))

  async function load() { return readPage(false) }

  async function readPage(forceAutomatic: boolean) {
    const previousSummaries = { ...summaries.value }
    const requestVersion = ++version
    controller?.abort()
    controller = new AbortController()
    const signal = controller.signal
    const user = currentOwner()
    const ids = [...new Set(accounts.value.filter(supported).map(account => account.id))]
    const visible = new Set(ids)
    summaries.value = Object.fromEntries(Object.entries(summaries.value).filter(([id]) => visible.has(Number(id))))
    if (!user || sessionInvalidated || ids.length === 0) { loading.value = false; unavailable.value = false; return }
    loading.value = true
    unavailable.value = false
    try {
      // Bound concurrent cache reads and respect the native per-request limit.
      for (let i = 0; i < ids.length; i += 100) {
        const results = await api.summaries(ids.slice(i, i + 100), signal)
        if (disposed || signal.aborted || requestVersion !== version || currentOwner() !== user) return
        for (const summary of results) if (visible.has(summary.account_id)) summaries.value[summary.account_id] = summary
      }
      // Read manual bindings first. Never silently replace an existing or invalidated binding.
      const automatic = ids.filter(id => summaries.value[id]?.status === 'unbound' && !summaries.value[id]?.source_id)
      let next = 0
      const valid = () => !disposed && !signal.aborted && requestVersion === version && currentOwner() === user
      await Promise.all(Array.from({ length: Math.min(2, automatic.length) }, async () => {
        while (valid() && next < automatic.length) {
          const id = automatic[next++]
          const previous = previousSummaries[id]?.automatic ? previousSummaries[id] : summaries.value[id]
          summaries.value[id] = { ...previous, automatic: true }
          try {
            const result = await api.automatic(id, forceAutomatic, signal)
            if (valid()) {
              summaries.value[id] = { ...result, binding_revision: summaries.value[id].binding_revision }
              if (forceAutomatic && (result.error || result.status === 'error')) refreshFailed.value = true
            }
          } catch (cause) {
            if (valid()) {
              const key = balanceErrorKey(cause)
              const invalidated = key === 'sourcesBalance.errors.conflict' ||
                [401, 403, 404].includes((cause as { status?: number })?.status ?? 0)
              const balance = invalidated ? null : summaries.value[id].balance
              summaries.value[id] = { ...summaries.value[id], status: 'error', balance, stale: Boolean(balance),
                ...(invalidated ? { auto_refresh: false, next_check_at: undefined } : {}),
                error: key === 'sourcesBalance.errors.queryTimeout' ? 'upstream_timeout'
                  : invalidated ? 'wallet_config_changed' : 'upstream_unavailable' }
              if (forceAutomatic) refreshFailed.value = true
            }
          }
        }
      }))
    } catch {
      if (!disposed && !signal.aborted && requestVersion === version && currentOwner() === user) unavailable.value = true
    } finally {
      if (requestVersion === version) loading.value = false
    }
  }

  async function refreshPage() {
    if (refreshing.value || !supportsRefresh.value) return
    const user = currentOwner()
    const page = accounts.value.map(account => account.id).join(',')
    const representative = new Map<string, number>()
    for (const account of accounts.value) {
      const summary = summaries.value[account.id]
      if (summary?.source_id && supported(account) && summary.status !== 'rebind_required') representative.set(summary.source_id, account.id)
    }
    const ids = [...representative.values()]
    const failedSources = new Map<string, string>()
    refreshing.value = true
    refreshFailed.value = false
    refreshController = new AbortController()
    const signal = refreshController.signal
    try {
      for (let i = 0; i < ids.length; i += 100) {
        if (disposed || signal.aborted || currentOwner() !== user || accounts.value.map(account => account.id).join(',') !== page) return
        const result = await api.refreshAccounts(ids.slice(i, i + 100), signal)
        for (const item of result) if (item.error || ['error', 'unit_unconfirmed'].includes(item.status)) {
          refreshFailed.value = true
          if (item.source_id) failedSources.set(item.source_id, item.error || 'upstream_unavailable')
        }
      }
    } catch {
      if (!signal.aborted && currentOwner() === user) refreshFailed.value = true
    } finally {
      if (!disposed && !signal.aborted && currentOwner() === user && accounts.value.map(account => account.id).join(',') === page) {
        await readPage(true)
        if (!signal.aborted && currentOwner() === user && accounts.value.map(account => account.id).join(',') === page) {
          for (const summary of Object.values(summaries.value)) {
            const error = summary.source_id && failedSources.get(summary.source_id)
            if (error && summary.status !== 'rebind_required') summaries.value[summary.account_id] = { ...summary, error, stale: Boolean(summary.balance) }
          }
        }
      }
      refreshing.value = false
    }
  }

  function apply(summary: AccountBalance) {
    if (accounts.value.some(account => account.id === summary.account_id)) summaries.value[summary.account_id] = summary
    // A changed/shared source can affect several rows; reread cache without
    // performing additional upstream queries.
    void load()
  }

  watch(accounts, () => {
    // Native account DTOs intentionally redact model secrets. A replaced row
    // may therefore contain a new credential with identical visible fields;
    // discard its old wallet amount before asking Portal to recheck identity.
    summaries.value = {}
    refreshController?.abort()
    void load()
  }, { immediate: true })
  watch(currentOwner, () => {
    sessionInvalidated = false
    summaries.value = {}
    refreshController?.abort()
    void load()
  })
  const sessionChanged = (event: StorageEvent) => {
    if (identityChanged(event, currentOwner())) {
      sessionInvalidated = true
      ++version; controller?.abort(); refreshController?.abort(); summaries.value = {}; loading.value = false
    }
  }
  window.addEventListener('storage', sessionChanged)
  // Refresh visible snapshots, never upstream wallets. The server owns the
  // five-minute schedule even after every browser tab has been closed.
  const readVisibleSnapshots = () => {
    if (document.visibilityState === 'visible' && !loading.value && !refreshing.value) void load()
  }
  const snapshotTimer = window.setInterval(readVisibleSnapshots, 60_000)
  document.addEventListener('visibilitychange', readVisibleSnapshots)
  onScopeDispose(() => {
    disposed = true; ++version; controller?.abort(); refreshController?.abort()
    window.clearInterval(snapshotTimer)
    document.removeEventListener('visibilitychange', readVisibleSnapshots)
    window.removeEventListener('storage', sessionChanged)
  })
  return { summaries, loading, unavailable, refreshing, refreshFailed, supportsRefresh, load, refreshPage, apply, supported }
}
