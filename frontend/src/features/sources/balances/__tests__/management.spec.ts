import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import zh from '@/i18n/locales/zh/sourcesBalance'
import WalletManagementDrawer from '../WalletManagementDrawer.vue'
import type { WalletSettingsView } from '../types'

const api = vi.hoisted(() => ({ get: vi.fn(), save: vi.fn(), unlink: vi.fn(), verify: vi.fn(), preview: vi.fn(), summaries: vi.fn() }))
vi.mock('../api', () => ({ walletManagementAPI: api, balanceAPI: { summaries: api.summaries } }))
vi.mock('../BalanceDrawer.vue', () => ({ default: { name: 'BalanceDrawerStub', emits: ['changed'], template: '<div data-testid="query-panel">Query panel</div>' } }))
vi.mock('@/utils/format', () => ({ formatDateTime: (value: string) => value }))
vi.mock('vue-i18n', async importOriginal => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string, args: Record<string, unknown> = {}) => {
    const value = key.split('.').reduce<unknown>((item, part) => (item as Record<string, unknown>)?.[part], zh)
    return String(value ?? key).replace(/\{(\w+)\}/g, (_, name: string) => String(args[name] ?? name))
  } })
}))


function fixture(): WalletSettingsView {
  return { wallet: { id: 'sub:fixture', revision: 2, name: '秋 · Vulcan', provider: 'sub', representative_id: 1,
    alert: { enabled: false, threshold: '100', unit: 'USD', recipients: [], use_default_recipients: true, repeat_hours: 0, notify_recovery: false } },
    balance: { status: 'ready', remaining: '2162.48', unit: 'USD', checked_at: '2026-10-25T06:00:00Z', stale: false },
    members: [{ id: 1, name: 'Plus', platform: 'openai', site: 'wallet.example', wallet_id: 'sub:fixture', verified: true, needs_confirmation: false }],
    candidates: [{ id: 1, name: 'Plus', platform: 'openai', site: 'wallet.example', wallet_id: 'sub:fixture', verified: true, needs_confirmation: false },
      { id: 2, name: 'Pro', platform: 'openai', site: 'direct.example', verified: true, needs_confirmation: false },
      { id: 3, name: 'Other wallet', platform: 'openai', site: 'wallet.example', wallet_id: 'sub:other', verified: true, needs_confirmation: false }],
    wallets: [], default_recipients: ['admin@example.invalid'], deliveries: [], bridge_ready: true }
}
function render() {
  return mount(WalletManagementDrawer, { props: { account: { id: 1, name: 'Plus', type: 'apikey', platform: 'openai' } },
    global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh } })],
      stubs: { BaseDialog: { template: '<section><slot /><slot name="footer" /></section>' } } } })
}
describe('shared wallet management', () => {
  beforeEach(() => {
    Object.values(api).forEach(mock => mock.mockReset())
    api.get.mockResolvedValue(fixture())
    api.save.mockResolvedValue(fixture().wallet)
    api.summaries.mockResolvedValue([{ account_id: 1, supported: true, status: 'ready', wallet_id: 'sub:fixture' }])
  })
  it('saves explicit alert settings without leaking server-only alarm state', async () => {
    const value = fixture()
    Object.assign(value.wallet, { alarm: { low: true }, updated_at: 'server-only' })
    api.get.mockResolvedValue(value)
    const wrapper = render(); await flushPromises()
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('input[type="number"]').setValue('150')
    await wrapper.findAll('button').find(button => button.text() === '保存设置')!.trigger('click')
    await flushPromises()
    const sent = api.save.mock.calls[0][1]
    expect(sent.alert.enabled).toBe(true)
    expect(sent.alert.threshold).toBe('150')
    expect(sent.members).toEqual([1])
    expect(sent.alarm).toBeUndefined()
    expect(sent.updated_at).toBeUndefined()
    expect(wrapper.text()).toContain('设置已保存')
    wrapper.unmount()
  })
  it('requires shared-wallet confirmation and disables conflicting channels', async () => {
    const wrapper = render(); await flushPromises()
    await wrapper.get('#wallet-tab-members').trigger('click')
    const choices = wrapper.findAll('#wallet-panel-members input[type="checkbox"]')
    expect(choices[2].attributes('disabled')).toBeDefined()
    await choices[1].setValue(true)
    const save = wrapper.findAll('button').find(button => button.text() === '保存设置')!
    expect(save.attributes('disabled')).toBeDefined()
    await wrapper.findAll('#wallet-panel-members input[type="checkbox"]').at(-1)!.setValue(true)
    await save.trigger('click'); await flushPromises()
    expect(api.save.mock.calls[0][1].members).toEqual([1, 2])
    expect(api.save.mock.calls[0][1].confirm_shared).toBe(true)
    wrapper.unmount()
  })
  it('previews mail without sending or saving an alert rule', async () => {
    api.preview.mockResolvedValue({ subject: 'Preview', html: '<p>Example only</p>' })
    const wrapper = render(); await flushPromises()
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.findAll('button').find(button => button.text().includes('查看预览'))!.trigger('click')
    await flushPromises()
    expect(wrapper.get('iframe').attributes('sandbox')).toBe('')
    expect(wrapper.get('iframe').attributes('srcdoc')).toContain('Example only')
    expect(api.save).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('can close during a slow representative verification without late updates', async () => {
    const value = fixture(); value.members.push(value.candidates[1]); api.get.mockResolvedValue(value)
    let finish: (value: unknown) => void = () => {}
    api.verify.mockImplementation(() => new Promise(resolve => { finish = resolve }))
    const wrapper = render(); await flushPromises()
    await wrapper.get('#wallet-tab-query').trigger('click')
    await wrapper.findAll('button').find(button => button.text() === '验证此渠道余额')!.trigger('click')
    expect(wrapper.text()).toContain('最长等待 6 分钟')
    const signal = api.verify.mock.calls[0][1] as AbortSignal
    const close = wrapper.findAll('button').find(button => button.text() === '关闭余额配置')!
    expect(close.attributes('disabled')).toBeUndefined()
    await close.trigger('click'); expect(wrapper.emitted('close')).toHaveLength(1)
    await wrapper.setProps({ account: null }); expect(signal.aborted).toBe(true)
    finish({ status: 'ready', remaining: '20', unit: 'USD' }); await flushPromises()
    expect(wrapper.emitted('changed')).toBeUndefined()
    wrapper.unmount()
  })
  it('enrolls a first wallet after successful raw verification without losing the result', async () => {
    const value = fixture()
    value.wallet.id = ''; value.wallet.revision = 0; value.wallet.alert.unit = ''
    value.balance = { status: 'pending', stale: false }
    value.members[0].wallet_id = undefined; value.members[0].verified = false
    value.candidates[0] = value.members[0]
    api.get.mockResolvedValueOnce(value)
    api.verify.mockResolvedValue({ status: 'ready', remaining: '500', unit: 'USD', checked_at: '2026-10-25T06:01:00Z', stale: false })
    const wrapper = render(); await flushPromises()
    const save = wrapper.findAll('button').find(button => button.text() === '保存设置')!
    expect(save.attributes('disabled')).toBeDefined()
    await wrapper.get('#wallet-tab-query').trigger('click')
    await wrapper.get('[data-testid="wallet-verify-channel"]').trigger('click'); await flushPromises()
    expect(api.verify).toHaveBeenCalledWith(1, expect.any(AbortSignal))
    expect(wrapper.text()).toContain('500.00 USD')
    expect(save.attributes('disabled')).toBeUndefined()
    await save.trigger('click'); await flushPromises()
    expect(api.save.mock.calls[0][1]).toMatchObject({ id: '', revision: 0, representative_id: 1, members: [1], alert: { enabled: false, unit: 'USD' } })
    wrapper.unmount()
  })
  it('joins a verified existing wallet without querying the new member independently', async () => {
    const value = fixture()
    value.wallet.id = ''; value.wallet.revision = 0; value.wallet.alert.unit = ''
    value.balance = { status: 'pending', stale: false }
    value.members[0].wallet_id = undefined; value.members[0].verified = false
    value.candidates[0] = value.members[0]
    const target = fixture()
    target.wallet.id = 'sub:other'; target.wallet.name = 'Selected wallet'; target.wallet.revision = 7; target.wallet.representative_id = 3
    target.balance = { status: 'ready', remaining: '876', unit: 'USD', checked_at: '2026-10-25T06:01:00Z', stale: false }
    target.members = [value.candidates[2]]; target.candidates = value.candidates
    value.wallets = [target.wallet]
    api.get.mockResolvedValueOnce(value).mockResolvedValueOnce(target)
    const wrapper = render(); await flushPromises()
    await wrapper.get('#wallet-tab-members').trigger('click')
    await wrapper.get('#wallet-panel-members select').setValue('sub:other'); await flushPromises()
    const save = wrapper.findAll('button').find(button => button.text() === '保存设置')!
    expect(save.attributes('disabled')).toBeDefined()
    expect(wrapper.text()).toContain('876.00 USD')
    await wrapper.findAll('#wallet-panel-members input[type="checkbox"]').at(-1)!.setValue(true)
    expect(save.attributes('disabled')).toBeUndefined()
    await save.trigger('click'); await flushPromises()
    expect(api.save.mock.calls[0][1]).toMatchObject({ id: 'sub:other', revision: 7, representative_id: 3, members: [3, 1], confirm_shared: true })
    expect(api.verify).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('requires a new representative result instead of reusing the old displayed balance', async () => {
    const value = fixture(); value.members.push(value.candidates[1]); value.candidates[1].wallet_id = value.wallet.id
    api.get.mockResolvedValueOnce(value)
    api.verify.mockResolvedValue({ status: 'ready', remaining: '75', unit: 'USD', checked_at: '2026-10-25T06:01:00Z', stale: false })
    const wrapper = render(); await flushPromises()
    await wrapper.get('#wallet-tab-query').trigger('click')
    await wrapper.get('#wallet-panel-query select').setValue(2)
    expect(wrapper.text()).not.toContain('2,162.48')
    await wrapper.get('#wallet-tab-members').trigger('click')
    await wrapper.findAll('#wallet-panel-members input[type="checkbox"]').at(-1)!.setValue(true)
    const save = wrapper.findAll('button').find(button => button.text() === '保存设置')!
    expect(save.attributes('disabled')).toBeDefined()
    await wrapper.get('#wallet-tab-query').trigger('click')
    await wrapper.get('[data-testid="wallet-verify-channel"]').trigger('click'); await flushPromises()
    expect(api.verify).toHaveBeenCalledWith(2, expect.any(AbortSignal))
    expect(wrapper.text()).toContain('75.00 USD')
    expect(save.attributes('disabled')).toBeUndefined()
    await save.trigger('click'); await flushPromises()
    expect(api.save.mock.calls[0][1]).toMatchObject({ id: value.wallet.id, representative_id: 2, members: [1, 2], confirm_shared: true })
    wrapper.unmount()
  })
  it('reconfirms a single changed channel after raw verification while preserving its wallet and rule', async () => {
    const value = fixture(); value.wallet.alert.enabled = true
    value.balance = { status: 'rebind_required', error: 'wallet_rebind_required', stale: false }
    value.members[0].needs_confirmation = true; value.candidates[0] = value.members[0]
    api.get.mockResolvedValueOnce(value)
    api.verify.mockResolvedValue({ status: 'ready', remaining: '80', unit: 'USD', checked_at: '2026-10-25T06:01:00Z', stale: false })
    const wrapper = render(); await flushPromises()
    const save = wrapper.findAll('button').find(button => button.text() === '保存设置')!
    expect(save.attributes('disabled')).toBeDefined()
    await wrapper.get('#wallet-tab-query').trigger('click')
    expect(wrapper.find('#wallet-panel-query select').exists()).toBe(false)
    await wrapper.get('[data-testid="wallet-verify-channel"]').trigger('click'); await flushPromises()
    expect(save.attributes('disabled')).toBeUndefined()
    await save.trigger('click'); await flushPromises()
    expect(api.save.mock.calls[0][1]).toMatchObject({ id: value.wallet.id, revision: value.wallet.revision, members: [1], confirm_shared: true, alert: { enabled: true, threshold: '100', unit: 'USD' } })
    expect(api.unlink).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('disables an unchanged shared rule during identity or upstream failures without confirming membership', async () => {
    const value = fixture(); value.wallet.alert.enabled = true
    value.members.push(value.candidates[1]); value.candidates[1].wallet_id = value.wallet.id
    value.members.forEach(candidate => { candidate.needs_confirmation = true })
    value.candidates[0] = value.members[0]
    value.balance = { status: 'rebind_required', error: 'wallet_rebind_required', stale: true }
    api.get.mockResolvedValueOnce(value)
    const wrapper = render(); await flushPromises()
    expect(wrapper.get('[role="switch"]').attributes('disabled')).toBeUndefined()
    await wrapper.get('[role="switch"]').trigger('click')
    const save = wrapper.findAll('button').find(button => button.text() === '保存设置')!
    expect(save.attributes('disabled')).toBeUndefined()
    await save.trigger('click'); await flushPromises()
    expect(api.save.mock.calls[0][1]).toMatchObject({ id: value.wallet.id, members: [1, 2], confirm_shared: false, alert: { enabled: false } })
    expect(api.verify).not.toHaveBeenCalled()
    expect(api.unlink).not.toHaveBeenCalled()
    wrapper.unmount()
  })
  it('does not convert an initially checked shared membership into fresh confirmation intent', async () => {
    const value = fixture(); value.members.push(value.candidates[1]); value.candidates[1].wallet_id = value.wallet.id
    api.get.mockResolvedValueOnce(value)
    const wrapper = render(); await flushPromises()
    await wrapper.findAll('button').find(button => button.text() === '保存设置')!.trigger('click'); await flushPromises()
    expect(api.save.mock.calls[0][1].confirm_shared).toBe(false)
    wrapper.unmount()
  })
  it('keeps draft members and reminder edits when the query panel refreshes the saved wallet', async () => {
    const value = fixture()
    api.get.mockResolvedValue(value)
    const wrapper = render(); await flushPromises()
    await wrapper.get('[role="switch"]').trigger('click')
    await wrapper.get('input[type="number"]').setValue('150')
    await wrapper.get('#wallet-tab-members').trigger('click')
    await wrapper.findAll('#wallet-panel-members input[type="checkbox"]')[1].setValue(true)
    await wrapper.findAll('#wallet-panel-members input[type="checkbox"]').at(-1)!.setValue(true)
    await wrapper.get('#wallet-tab-query').trigger('click')
    wrapper.findComponent({ name: 'BalanceDrawerStub' }).vm.$emit('changed', { account_id: 1, status: 'ready', supported: true,
      provider: 'sub', wallet_id: value.wallet.id, representative_id: 1, binding_revision: 1, shared_count: 1,
      balance: { remaining: '600', unit: 'USD', checked_at: '2026-10-25T06:01:00Z' }, stale: false })
    await flushPromises()
    expect(wrapper.text()).toContain('600.00 USD')
    await wrapper.findAll('button').find(button => button.text() === '保存设置')!.trigger('click'); await flushPromises()
    expect(api.save.mock.calls[0][1]).toMatchObject({ members: [1, 2], confirm_shared: true, alert: { enabled: true, threshold: '150' } })
    wrapper.unmount()
  })
})
