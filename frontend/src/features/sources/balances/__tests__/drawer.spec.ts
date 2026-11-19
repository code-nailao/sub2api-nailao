import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh/sourcesBalance'
import BalanceDrawer from '../BalanceDrawer.vue'
import type { AccountBalance, BalanceSource } from '../types'

vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key.split('.').reduce<unknown>((value, part) => (value as Record<string, unknown>)?.[part], zh) ?? key })
}))
vi.mock('@/utils/format', () => ({ formatRelativeTime: () => '刚刚', formatDateTime: (value: string) => value }))

const api = vi.hoisted(() => ({ automatic: vi.fn(), walletConfiguration: vi.fn(), testWallet: vi.fn(), saveWallet: vi.fn(), sources: vi.fn(), summaries: vi.fn(), saveSource: vi.fn(), refreshSource: vi.fn(), bind: vi.fn(), unbind: vi.fn() }))
vi.mock('../api', () => ({ balanceAPI: api }))
const initial: AccountBalance = { account_id: 7, supported: true, status: 'unbound', binding_revision: 0,
  shared_count: 0, balance: null, last_checked_at: null, stale: false }
const source: BalanceSource = { id: 'wallet1', name: 'Existing wallet', origin: 'https://upstream.example', account_id: 42,
  threshold: '', threshold_unit: '', last_balance: { account_id: 42, remaining: '100.25', used: '0', unit: 'CNY', checked_at: '2026-10-14T00:00:00Z' },
  last_checked_at: '', next_check_at: '', status: 'ok', alert_pending: false, updated_by: 1, revision: 3 }
function render() {
  return mount(BalanceDrawer, { props: { account: { id: 7, name: 'API account', type: 'apikey', platform: 'openai' }, summary: initial },
    global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh } })],
      stubs: { BaseDialog: { template: '<section><slot /><slot name="footer" /></section>' } } } })
}
describe('balance drawer', () => {
  beforeEach(() => {
    Object.values(api).forEach(mock => mock.mockReset())
    api.walletConfiguration.mockResolvedValue({ url: '', call_url: 'https://direct.example/v1', revision: 'revision-1', supported: true });
    api.sources.mockResolvedValue([source]); api.summaries.mockResolvedValue([initial])
    api.automatic.mockResolvedValue({ ...initial, automatic: true, status: "automatic_unavailable" })
    api.bind.mockResolvedValue({ ...initial, status: 'ready', source_id: source.id, source_name: source.name, binding_revision: 1, balance: source.last_balance })
  })
  it('keeps the close action available and aborts polling during a slow query', async () => {
    const wrapper = render()
    await flushPromises()
    let finish: (value: AccountBalance) => void = () => {}
    api.automatic.mockImplementation(() => new Promise(resolve => { finish = resolve }))
    await wrapper.findAll('button').find(button => button.text() === '查询余额')!.trigger('click')
    expect(wrapper.text()).toContain('最长等待 6 分钟')
    const cancel = wrapper.findAll('button').find(button => button.text() === '取消')!
    expect(cancel.attributes('disabled')).toBeUndefined()
    const signal = api.automatic.mock.calls.at(-1)![2] as AbortSignal
    await cancel.trigger('click')
    expect(wrapper.emitted('close')).toHaveLength(1)
    await wrapper.setProps({ account: null })
    expect(signal.aborted).toBe(true)
    finish({ ...initial, automatic: true, status: 'ready', balance: source.last_balance })
    await flushPromises()
    expect(wrapper.emitted('changed')).toBeUndefined()
    wrapper.unmount()
  })
  it('shows successful automatic enrollment and the next refresh time', async () => {
    api.automatic.mockResolvedValue({ ...initial, automatic: true, status: 'ready', balance: source.last_balance,
      auto_refresh: true, next_check_at: '2026-10-25T01:05:00Z' })
    const wrapper = render()
    await flushPromises()
    expect(wrapper.get('[data-testid="wallet-refresh-schedule"]').text()).toContain('每 5 分钟')
    expect(wrapper.text()).toContain('下次查询')
    wrapper.unmount()
  })
  it('shows the automatic wallet without asking for another configuration', async () => {
    api.automatic.mockResolvedValue({ ...initial, automatic: true, status: 'ready', balance: source.last_balance })
    const wrapper = render(); await flushPromises()
    expect(wrapper.get('[data-testid="account-balance-cell"]').text()).toContain('100')
    expect(wrapper.get('[data-testid="account-balance-cell"]').attributes('title')).toContain('100.25 CNY')
    expect(wrapper.find('select').exists()).toBe(false)
    expect(wrapper.find('input[type="password"]').exists()).toBe(false)
    expect(api.saveSource).not.toHaveBeenCalled()
    expect(api.bind).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('does not query automatically when a manual source is already bound', async () => {
    api.summaries.mockResolvedValue([{ ...initial, source_id: source.id, status: 'ready', balance: source.last_balance }])
    const wrapper = render(); await flushPromises()
    expect(api.automatic).not.toHaveBeenCalled()
    expect(wrapper.find('select').exists()).toBe(true)
    wrapper.unmount()
  })
  it('immediately returns to automatic lookup after unlinking a manual source', async () => {
    api.summaries.mockResolvedValueOnce([{ ...initial, source_id: source.id, status: 'ready', balance: source.last_balance }])
    api.unbind.mockResolvedValue({ ...initial, binding_revision: 3 })
    api.automatic.mockResolvedValue({ ...initial, automatic: true, status: 'ready', balance: source.last_balance })
    const wrapper = render(); await flushPromises()
    expect(api.automatic).not.toHaveBeenCalled()
    await wrapper.findAll('button').find(button => button.text() === '解除 New API 关联')!.trigger('click')
    await flushPromises()
    expect(api.unbind).toHaveBeenCalledTimes(1)
    expect(api.automatic).toHaveBeenCalledTimes(1)
    expect(wrapper.text()).toContain('默认使用此账号现有配置')
    expect(wrapper.find('select').exists()).toBe(false)
    wrapper.unmount()
  })
  it('explicitly binds an existing source using both revisions without querying the upstream', async () => {
    const wrapper = render(); await flushPromises()
    await wrapper.get('[data-testid="balance-mode-new"]').setValue()
    await wrapper.get('select').setValue(source.id)
    const button = wrapper.findAll('button').find(button => button.text() === '关联到此账号')!
    await button.trigger('click'); await flushPromises()
    expect(api.bind).toHaveBeenCalledWith(7, source, 0)
    expect(api.refreshSource).not.toHaveBeenCalled()
    expect(wrapper.emitted('changed')).toHaveLength(1)
    wrapper.unmount()
  })
  it('saves a PAT source but refuses to bind it when verification fails', async () => {
    api.saveSource.mockResolvedValue({ ...source, status: 'pending' })
    api.refreshSource.mockResolvedValue({ ...source, status: 'error', error: 'upstream_credential_invalid' })
    const wrapper = render(); await flushPromises()
    await wrapper.get('[data-testid="balance-mode-new"]').setValue()
    await wrapper.findAll('button').find(button => button.text() === '新建来源')!.trigger('click')
    const inputs = wrapper.get('form').findAll('input')
    await inputs[0].setValue('New wallet'); await inputs[1].setValue('https://upstream.example')
    await inputs[2].setValue('personal-test-token')
    await wrapper.get('form').trigger('submit'); await flushPromises()
    expect(api.saveSource).toHaveBeenCalledTimes(1)
    expect(api.refreshSource).toHaveBeenCalledWith(source.id, expect.any(AbortSignal))
    expect(api.bind).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('个人访问令牌无效')
    expect(wrapper.html()).not.toContain('personal-test-token')
    wrapper.unmount()
  })
  it('does not continue refresh or binding after the drawer is removed mid-save', async () => {
    let finish: (value: BalanceSource) => void = () => {}
    api.saveSource.mockImplementation(() => new Promise(resolve => { finish = resolve }))
    const wrapper = render(); await flushPromises()
    await wrapper.get('[data-testid="balance-mode-new"]').setValue()
    await wrapper.findAll('button').find(button => button.text() === '新建来源')!.trigger('click')
    const inputs = wrapper.get('form').findAll('input')
    await inputs[0].setValue('New'); await inputs[1].setValue(source.origin); await inputs[2].setValue('personal-token')
    await wrapper.get('form').trigger('submit')
    wrapper.unmount(); finish(source); await flushPromises()
    expect(api.refreshSource).not.toHaveBeenCalled()
    expect(api.bind).not.toHaveBeenCalled()
  })
  it('preserves an open drawer for same-user profile changes and closes it on identity replacement', async () => {
    const wrapper = render(); await flushPromises()
    window.dispatchEvent(new StorageEvent('storage', { key: 'auth_user', oldValue: '{"id":1}', newValue: '{"id":1,"name":"New"}' }))
    expect(wrapper.emitted('close')).toBeUndefined()
    window.dispatchEvent(new StorageEvent('storage', { key: 'auth_user', oldValue: '{"id":1}', newValue: '{"id":2}' }))
    expect(wrapper.emitted('close')).toHaveLength(1)
    wrapper.unmount()
  })
  it('requires a successful test, normalizes /v1, and invalidates verification on every URL edit', async () => {
    api.testWallet.mockResolvedValue({ url: 'https://sub.example.com', wallet: { status: 'ready', remaining: '0', unit: 'USD', stale: false }, verification_token: 'proof' })
    const wrapper = render(); await flushPromises()
    await wrapper.get('[data-testid="balance-mode-sub"]').setValue()
    await wrapper.get('[data-testid="wallet-url"]').setValue('http://sub.example.com')
    expect(wrapper.get('[data-testid="wallet-test"]').attributes('disabled')).toBeDefined()
    expect(wrapper.get('[data-testid="wallet-save"]').attributes('disabled')).toBeDefined()
    await wrapper.get('[data-testid="wallet-url"]').setValue('https://sub.example.com/v1')
    await wrapper.get('[data-testid="wallet-form"]').trigger('submit'); await flushPromises()
    expect(api.testWallet).toHaveBeenCalledWith(7, 'https://sub.example.com', 'revision-1', expect.any(AbortSignal))
    expect(api.saveWallet).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="wallet-result"]').text()).toContain('0.00 USD')
    expect(wrapper.get('[data-testid="wallet-save"]').attributes('disabled')).toBeUndefined()
    await wrapper.get('[data-testid="wallet-url"]').setValue('https://other.example.com')
    await wrapper.get('[data-testid="wallet-url"]').setValue('https://sub.example.com')
    expect(wrapper.get('[data-testid="wallet-save"]').attributes('disabled')).toBeDefined()
    expect(wrapper.find('[data-testid="wallet-result"]').exists()).toBe(false)
    wrapper.unmount()
  })
  it('saves only the tested URL, revision and proof then reloads the real balance', async () => {
    api.testWallet.mockResolvedValue({ url: 'https://sub.example.com', wallet: { status: 'ready', remaining: '-0.01', unit: 'USD', stale: false }, verification_token: 'proof' })
    api.saveWallet.mockResolvedValue({ url: 'https://sub.example.com', call_url: 'https://direct.example/v1', revision: 'revision-2', supported: true })
    const wrapper = render(); await flushPromises()
    await wrapper.get('[data-testid="balance-mode-sub"]').setValue()
    await wrapper.get('[data-testid="wallet-url"]').setValue('https://sub.example.com')
    await wrapper.get('[data-testid="wallet-form"]').trigger('submit'); await flushPromises()
    await wrapper.get('[data-testid="wallet-save"]').trigger('click'); await flushPromises()
    expect(api.saveWallet).toHaveBeenCalledWith(7, 'https://sub.example.com', 'revision-1', 'proof', expect.any(AbortSignal))
    expect(api.automatic).toHaveBeenLastCalledWith(7, true, expect.any(AbortSignal))
    expect(api.bind).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('余额查询地址已保存')
    expect(wrapper.get('[data-testid="wallet-save"]').attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })
  it('switching modes never removes an existing New API binding', async () => {
    api.summaries.mockResolvedValue([{ ...initial, source_id: source.id, status: 'ready', balance: source.last_balance }])
    const wrapper = render(); await flushPromises()
    await wrapper.get('[data-testid="balance-mode-sub"]').setValue()
    expect(wrapper.find('[data-testid="wallet-url"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('先解除关联')
    expect(api.unbind).not.toHaveBeenCalled()
    expect(api.testWallet).not.toHaveBeenCalled()
    expect(api.saveWallet).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('does not save if a New API source was linked after testing', async () => {
    api.testWallet.mockResolvedValue({ url: 'https://sub.example.com', wallet: { status: 'ready', remaining: '1', unit: 'USD', stale: false }, verification_token: 'proof' })
    const wrapper = render(); await flushPromises()
    await wrapper.get('[data-testid="balance-mode-sub"]').setValue()
    await wrapper.get('[data-testid="wallet-url"]').setValue('https://sub.example.com')
    await wrapper.get('[data-testid="wallet-form"]').trigger('submit'); await flushPromises()
    api.summaries.mockResolvedValue([{ ...initial, source_id: source.id, status: 'ready' }])
    await wrapper.get('[data-testid="wallet-save"]').trigger('click'); await flushPromises()
    expect(api.saveWallet).not.toHaveBeenCalled()
    expect(api.unbind).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('先解除关联')
    wrapper.unmount()
  })
  it.each(['ready', 'rebind_required'] as const)('saves a tested query URL for a managed Sub wallet with status %s without unlinking it', async status => {
    const managed: AccountBalance = { ...initial, status, automatic: true, provider: 'sub', wallet_id: 'sub:managed',
      representative_id: 7, binding_revision: 3, shared_count: 2, balance: status === 'ready' ? source.last_balance! : null }
    const rebind: AccountBalance = { ...managed, status: 'rebind_required', balance: null, error: 'wallet_rebind_required' }
    api.summaries.mockResolvedValue([managed])
    api.testWallet.mockResolvedValue({ url: 'https://sub.example.com', wallet: { status: 'ready', remaining: '5', unit: 'USD', stale: false }, verification_token: 'proof' })
    api.saveWallet.mockImplementation(async () => {
      api.summaries.mockResolvedValue([rebind])
      return { url: 'https://sub.example.com', call_url: 'https://direct.example/v1', revision: 'revision-2', supported: true }
    })
    api.automatic.mockResolvedValue(rebind)
    const wrapper = render(); await flushPromises()
    await wrapper.get('[data-testid="balance-mode-sub"]').setValue()
    expect(wrapper.find('[data-testid="unlink-before-sub"]').exists()).toBe(false)
    await wrapper.get('[data-testid="wallet-url"]').setValue('https://sub.example.com')
    await wrapper.get('[data-testid="wallet-form"]').trigger('submit'); await flushPromises()
    await wrapper.get('[data-testid="wallet-save"]').trigger('click'); await flushPromises()
    expect(api.saveWallet).toHaveBeenCalledWith(7, 'https://sub.example.com', 'revision-1', 'proof', expect.any(AbortSignal))
    expect(wrapper.emitted('changed')?.at(-1)?.[0]).toMatchObject({ wallet_id: 'sub:managed', provider: 'sub', status: 'rebind_required' })
    expect(api.bind).not.toHaveBeenCalled()
    expect(api.unbind).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('restores automatic lookup for a managed Sub representative while keeping canonical membership', async () => {
    const managed: AccountBalance = { ...initial, status: 'ready', automatic: true, provider: 'sub', wallet_id: 'sub:managed',
      representative_id: 7, binding_revision: 3, shared_count: 2, balance: source.last_balance! }
    const rebind: AccountBalance = { ...managed, status: 'rebind_required', balance: null, error: 'wallet_rebind_required' }
    api.summaries.mockResolvedValue([managed])
    api.walletConfiguration.mockResolvedValue({ url: 'https://sub.example.com', call_url: 'https://direct.example/v1', revision: 'revision-1', supported: true })
    api.saveWallet.mockImplementation(async () => {
      api.summaries.mockResolvedValue([rebind])
      return { url: '', call_url: 'https://direct.example/v1', revision: 'revision-2', supported: true }
    })
    api.automatic.mockResolvedValue(rebind)
    const wrapper = render(); await flushPromises()
    await wrapper.get('[data-testid="balance-mode-automatic"]').setValue()
    await wrapper.get('[data-testid="wallet-reset"]').trigger('click'); await flushPromises()
    expect(api.saveWallet).toHaveBeenCalledWith(7, '', 'revision-1', undefined, expect.any(AbortSignal))
    expect(wrapper.emitted('changed')?.at(-1)?.[0]).toMatchObject({ wallet_id: 'sub:managed', status: 'rebind_required' })
    expect(api.bind).not.toHaveBeenCalled()
    expect(api.unbind).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('retains canonical Sub ownership when a raw refresh reports an identity change', async () => {
    const managed: AccountBalance = { ...initial, status: 'rebind_required', automatic: true, provider: 'sub',
      wallet_id: 'sub:managed', representative_id: 7, binding_revision: 3, shared_count: 1 }
    api.summaries.mockResolvedValue([managed])
    api.automatic.mockResolvedValue({ ...initial, automatic: true, status: 'rebind_required', error: 'wallet_rebind_required' })
    const wrapper = render(); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '查询余额')!.trigger('click'); await flushPromises()
    expect(wrapper.emitted('changed')?.at(-1)?.[0]).toMatchObject({ wallet_id: 'sub:managed', provider: 'sub', status: 'rebind_required' })
    await wrapper.get('[data-testid="balance-mode-sub"]').setValue()
    expect(wrapper.find('[data-testid="wallet-url"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="unlink-before-sub"]').exists()).toBe(false)
    expect(api.unbind).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('drops a late test result after the selected account changes', async () => {
    let finish: (value: unknown) => void = () => {}
    api.testWallet.mockImplementation(() => new Promise(resolve => { finish = resolve }))
    const wrapper = render(); await flushPromises()
    await wrapper.get('[data-testid="balance-mode-sub"]').setValue()
    await wrapper.get('[data-testid="wallet-url"]').setValue('https://sub.example.com')
    await wrapper.get('[data-testid="wallet-form"]').trigger('submit')
    await wrapper.setProps({ account: { id: 8, name: 'Other', platform: 'openai', type: 'apikey' } }); await flushPromises()
    finish({ url: 'https://sub.example.com', wallet: { status: 'ready', remaining: '123', unit: 'USD' }, verification_token: 'proof' })
    await flushPromises()
    expect(wrapper.find('[data-testid="wallet-result"]').exists()).toBe(false)
    expect(api.saveWallet).not.toHaveBeenCalled()
    wrapper.unmount()
  })

})
