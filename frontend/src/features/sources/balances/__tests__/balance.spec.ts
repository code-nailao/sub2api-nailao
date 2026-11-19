import { afterEach, describe, expect, it, vi } from 'vitest'
import { effectScope, nextTick, ref, type EffectScope } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh/sourcesBalance'
import AccountBalanceCell from '../AccountBalanceCell.vue'
import { balanceErrorKey, compactAmount, decimalAmount, sourceVerified, unlimitedAmount } from '../presentation'
import { useAccountBalances } from '../useAccountBalances'
import type { AccountBalance, BalanceAPI, BalanceSource } from '../types'

vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key.split('.').reduce<unknown>((value, part) => (value as Record<string, unknown>)?.[part], zh) ?? key })
}))
vi.mock('@/utils/format', () => ({ formatRelativeTime: () => '刚刚', formatDateTime: (value: string) => value }))

function summary(id: number, source = 'wallet'): AccountBalance {
  return { account_id: id, supported: true, status: 'ready', source_id: source, source_name: source,
    binding_revision: 1, shared_count: 2, last_checked_at: '2026-10-14T00:00:00Z', stale: false,
    balance: { account_id: 99, remaining: '0', used: '12', unit: 'CNY', checked_at: '2026-10-14T00:00:00Z' } }
}
const source: BalanceSource = { id: 'a', name: 'Wallet', origin: 'https://example.com', account_id: 99,
  threshold: '', threshold_unit: '', last_balance: summary(1).balance, last_checked_at: '', next_check_at: '',
  status: 'ok', alert_pending: false, updated_by: 1, revision: 1 }

describe('balance values', () => {
  it('explains when a New API site requires an upstream user ID', () => {
    expect(balanceErrorKey({ error: 'upstream_user_id_required' })).toBe('sourcesBalance.errors.userIDRequired')
  })
  it('shows a timeout explicitly and preserves the last successful amount', async () => {
    const wrapper = mount(AccountBalanceCell, { props: { summary: {
      ...summary(1), status: 'error', error: 'upstream_timeout', stale: true
    }, supported: true } })
    expect(wrapper.text()).toContain('查询超时')
    expect(wrapper.text()).toContain('历史值')
    expect(wrapper.text()).toContain('0.00')
    await wrapper.setProps({ summary: { ...summary(1), status: 'error', balance: null, error: 'upstream_timeout' } })
    expect(wrapper.text()).toContain('查询超时')
    expect(wrapper.text()).not.toContain('0.00')
    expect(balanceErrorKey({ code: 'ECONNABORTED' })).toBe('sourcesBalance.errors.queryStillRunning')
    expect(balanceErrorKey({ error: 'upstream_timeout' })).toBe('sourcesBalance.errors.queryTimeout')
    wrapper.unmount()
  })
  it('uses legacy precision without floating point loss and preserves raw values', () => {
    for (const [raw, shown] of [['820.43634499', '820'], ['5893.27418033', '5893'],
      ['28.43634499', '28.44'], ['1.005', '1.01'], ['99.999', '100.00'], ['100.5', '101'],
      ['-100.5', '-101'], ['-0.0001', '0.00'], ['0', '0.00'], ['00099.1', '99.10'],
      ['999999999999999999999.9', '1000000000000000000000'], ['bad', '—']]) {
      expect(compactAmount(raw)).toBe(shown)
    }
    expect(decimalAmount('820.43634499')).toBe('820.43634499')
  })
  it('marks only values strictly above one million as unlimited before rounding', () => {
    for (const raw of ['1000000', '1000000.00000000', '999999.999999999', '-1000001', 'invalid', '0']) {
      expect(unlimitedAmount(raw)).toBe(false)
    }
    for (const raw of ['1000000.000000000000000001', '1000001', '999999192.1528215', '0001000000.01']) {
      expect(unlimitedAmount(raw)).toBe(true)
    }
  })
  it('renders compact badges, keeps precise tooltips and removes the unit for unlimited', async () => {
    const value = summary(1)
    value.balance = { ...value.balance!, remaining: '820.43634499', unit: 'USD' }
    const wrapper = mount(AccountBalanceCell, { props: { summary: value, supported: true } })
    expect(wrapper.text()).toContain('820')
    expect(wrapper.text()).not.toContain('820.43634499')
    expect(wrapper.attributes('title')).toContain('820.43634499 USD')
    await wrapper.trigger('click')
    expect(wrapper.emitted('configure')).toHaveLength(1)
    await wrapper.setProps({ summary: { ...value, balance: { ...value.balance, remaining: '1000000.00000000001' } } })
    expect(wrapper.text()).toContain('无限额度')
    expect(wrapper.text()).not.toContain('USD')
    expect(wrapper.attributes('title')).toContain('1,000,000.00000000001 USD')
    await wrapper.setProps({ summary: { ...value, status: 'error', balance: { ...value.balance, remaining: '1000001' } } })
    expect(wrapper.text()).toContain('历史值')
    expect(wrapper.text()).toContain('查询失败')
    await wrapper.setProps({ summary: { ...value, status: 'rebind_required' } })
    expect(wrapper.text()).toContain('需重新关联')
    expect(wrapper.attributes('title')).toBeUndefined()
    wrapper.unmount()
  })

  it('preserves exact decimal values, zero, negatives and sub-cent amounts', () => {
    expect(decimalAmount('12345678901234567890.120030')).toBe('12,345,678,901,234,567,890.12003')
    expect(decimalAmount('0')).toBe('0.00')
    expect(decimalAmount('-0.000000000012')).toBe('-0.000000000012')
    expect(decimalAmount('-0000.00')).toBe('0.00')
    expect(decimalAmount('not-money')).toBe('—')
  })
  it('requires a verified same upstream identity and explicit unit before binding', () => {
    expect(sourceVerified(source)).toBe(true)
    expect(sourceVerified({ ...source, status: 'pending' })).toBe(false)
    expect(sourceVerified({ ...source, last_balance: { ...source.last_balance!, account_id: 20 } })).toBe(false)
    expect(sourceVerified({ ...source, last_balance: { ...source.last_balance!, unit: '' } })).toBe(false)
  })
  it('retains failed balances, never invents USD, and hides amounts after identity changes', async () => {
    const wrapper = mount(AccountBalanceCell, { props: { summary: { ...summary(1), status: 'error' }, supported: true },
      global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh } })] } })
    expect(wrapper.text()).toContain('0.00')
    expect(wrapper.text()).toContain('CNY')
    expect(wrapper.text()).toContain('历史值')
    await wrapper.setProps({ summary: { ...summary(1), balance: { ...summary(1).balance!, unit: '' } } })
    expect(wrapper.text()).toContain('单位待确认')
    expect(wrapper.text()).not.toContain('USD')
    await wrapper.setProps({ summary: { ...summary(1), status: 'rebind_required' } })
    expect(wrapper.text()).toContain('需重新关联')
    expect(wrapper.text()).not.toContain('0.00')
    wrapper.unmount()
  })
})

describe('independent page balance reads', () => {
  const scopes: EffectScope[] = []
  afterEach(() => { scopes.splice(0).forEach(scope => scope.stop()) })
  function fixture(count = 2) {
    const rows = ref(Array.from({ length: count }, (_, i) => ({ id: i + 1, name: `Account ${i}`, type: 'apikey', platform: 'openai' })))
    const owner = ref<number | null>(1)
    const api: BalanceAPI = {
      walletConfiguration: vi.fn(), testWallet: vi.fn(), saveWallet: vi.fn(),
      automatic: vi.fn(),
      summaries: vi.fn(async ids => ids.map(id => summary(id))), sources: vi.fn(), saveSource: vi.fn(),
      refreshSource: vi.fn(), bind: vi.fn(), unbind: vi.fn(), refreshAccounts: vi.fn(async ids => ids.map(id => summary(id)))
    }
    const scope = effectScope(); scopes.push(scope)
    const state = scope.run(() => useAccountBalances(rows, () => owner.value, api))!
    return { rows, owner, api, state, scope }
  }
  it('automatically reads unbound accounts but preserves existing manual bindings', async () => {
    const { api, state } = fixture()
    await flushPromises()
    vi.mocked(api.summaries).mockResolvedValue([{ ...summary(1), source_id: '', status: 'unbound', balance: null }, summary(2)])
    vi.mocked(api.automatic).mockResolvedValue({ ...summary(1, ''), automatic: true })
    await state.load()
    expect(api.automatic).toHaveBeenCalledTimes(1)
    expect(vi.mocked(api.automatic).mock.calls[0][0]).toBe(1)
    expect(state.summaries.value[1].automatic).toBe(true)
    expect(state.summaries.value[2].source_id).toBe('wallet')
  })
  it('polls only saved snapshots while the page is visible and stops on disposal', async () => {
    vi.useFakeTimers()
    try {
      const { api, state, scope } = fixture(1)
      await flushPromises()
      vi.mocked(api.summaries).mockResolvedValue([{ ...summary(1), source_id: '', status: 'unbound', balance: null }])
      vi.mocked(api.automatic).mockResolvedValue({ ...summary(1, ''), automatic: true })
      await state.load()
      vi.mocked(api.automatic).mockClear()
      await vi.advanceTimersByTimeAsync(60_000)
      await flushPromises()
      expect(api.automatic).toHaveBeenCalledTimes(1)
      expect(vi.mocked(api.automatic).mock.calls[0][1]).toBe(false)
      expect(api.refreshAccounts).not.toHaveBeenCalled()
      scope.stop()
      await vi.advanceTimersByTimeAsync(120_000)
      expect(api.automatic).toHaveBeenCalledTimes(1)
    } finally { vi.useRealTimers() }
  })
  it('clears a previous automatic wallet when the server reports an identity conflict', async () => {
    const { api, state } = fixture(1)
    await flushPromises()
    vi.mocked(api.summaries).mockResolvedValue([{ ...summary(1), source_id: '', status: 'unbound', balance: null }])
    vi.mocked(api.automatic).mockResolvedValue({ ...summary(1, ''), automatic: true, auto_refresh: true })
    await state.load()
    expect(state.summaries.value[1].balance).not.toBeNull()
    vi.mocked(api.automatic).mockRejectedValue({ status: 409 })
    await state.load()
    expect(state.summaries.value[1].balance).toBeNull()
    expect(state.summaries.value[1].auto_refresh).toBe(false)
  })
  it('does not auto-replace a manual binding after a key change', async () => {
    const { api, state } = fixture()
    await flushPromises()
    vi.mocked(api.summaries).mockResolvedValue([{ ...summary(1), status: 'rebind_required' }])
    await state.load()
    expect(api.automatic).not.toHaveBeenCalled()
  })
  it('discards an automatic result arriving after account replacement', async () => {
    const { api, state, rows } = fixture(1)
    await flushPromises()
    vi.mocked(api.summaries).mockImplementation(async ids => ids.map(id => ({ ...summary(id), source_id: '', status: 'unbound', balance: null })))
    let finish!: (value: AccountBalance) => void
    vi.mocked(api.automatic).mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const old = state.load()
    await flushPromises()
    rows.value = []
    await nextTick()
    finish({ ...summary(1, ''), automatic: true })
    await old
    expect(state.summaries.value).toEqual({})
  })
  it('reads 1000 rows in bounded batches and refreshes a shared wallet just once', async () => {
    const { api, state } = fixture(1000)
    await flushPromises()
    expect(api.summaries).toHaveBeenCalledTimes(10)
    expect(Object.keys(state.summaries.value)).toHaveLength(1000)
    await state.refreshPage()
    expect(api.refreshAccounts).toHaveBeenCalledTimes(1)
    expect(vi.mocked(api.refreshAccounts).mock.calls[0][0]).toHaveLength(1)
  })
  it('does not apply late results after pagination or logout', async () => {
    const { api, state, rows, owner } = fixture()
    await flushPromises()
    let finish: (result: AccountBalance[]) => void = () => {}
    vi.mocked(api.summaries).mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const pending = state.load()
    rows.value = [{ id: 99, name: 'New page', type: 'apikey', platform: 'openai' }]
    await nextTick(); await flushPromises()
    finish([summary(1)]); await pending
    expect(Object.keys(state.summaries.value)).toEqual(['99'])
    vi.mocked(api.summaries).mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const other = state.load(); owner.value = null
    await nextTick(); finish([summary(99)]); await other
    expect(state.summaries.value).toEqual({})
  })
  it('keeps last successful cache on service failure and fan-outs failed source state', async () => {
    const { api, state } = fixture()
    await flushPromises()
    vi.mocked(api.summaries).mockRejectedValueOnce(new Error('unavailable'))
    await state.load()
    expect(state.unavailable.value).toBe(true)
    expect(state.summaries.value[1].balance?.remaining).toBe('0')
    vi.mocked(api.refreshAccounts).mockResolvedValueOnce([{ ...summary(1), error: 'refresh_in_progress_or_rate_limited' }])
    await state.refreshPage()
    expect(state.refreshFailed.value).toBe(true)
    expect(state.summaries.value[1].error).toBe('refresh_in_progress_or_rate_limited')
    expect(state.summaries.value[2].error).toBe('refresh_in_progress_or_rate_limited')
    expect(state.summaries.value[2].balance?.unit).toBe('CNY')
  })
  it('clears the visible amount immediately when an account row is replaced', async () => {
    const { api, state, rows } = fixture()
    await flushPromises()
    vi.mocked(api.summaries).mockImplementationOnce(() => new Promise(() => {}))
    rows.value = rows.value.map(row => ({ ...row, name: 'Edited API credentials' }))
    await nextTick()
    expect(state.summaries.value).toEqual({})
  })
  it('drops late results and prevents a stale store from refilling after cross-tab logout', async () => {
    const { api, state } = fixture()
    await flushPromises()
    let finish: (result: AccountBalance[]) => void = () => {}
    vi.mocked(api.summaries).mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const pending = state.load()
    window.dispatchEvent(new StorageEvent('storage', { key: 'auth_token', newValue: null }))
    finish([summary(1)]); await pending
    expect(state.summaries.value).toEqual({})
    const before = vi.mocked(api.summaries).mock.calls.length
    await state.load()
    expect(api.summaries).toHaveBeenCalledTimes(before)
  })
  it('keeps cache reads enabled after another tab updates the same user profile', async () => {
    const { api, state } = fixture()
    await flushPromises()
    window.dispatchEvent(new StorageEvent('storage', {
      key: 'auth_user', oldValue: JSON.stringify({ id: 1, username: 'Before' }),
      newValue: JSON.stringify({ id: 1, username: 'After' })
    }))
    const before = vi.mocked(api.summaries).mock.calls.length
    await state.load()
    expect(api.summaries).toHaveBeenCalledTimes(before + 1)
    expect(state.summaries.value[1].balance?.unit).toBe('CNY')
    window.dispatchEvent(new StorageEvent('storage', { key: 'auth_user', oldValue: '{"id":1}', newValue: '{"id":2}' }))
    expect(state.summaries.value).toEqual({})
  })
})
