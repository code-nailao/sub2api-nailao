import { defineComponent } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { Account, AdminGroup } from '@/types'

const mocks = vi.hoisted(() => ({ create: vi.fn(), update: vi.fn(), showError: vi.fn() }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: mocks.showError, showSuccess: vi.fn(), showWarning: vi.fn(), showInfo: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isSimpleMode: false }) }))
vi.mock('@/api/admin', () => ({ adminAPI: {
  accounts: {
    create: mocks.create, update: mocks.update,
    probeUpstreamBilling: vi.fn().mockResolvedValue({}),
    syncUpstreamModels: vi.fn().mockResolvedValue({ models: [], metadata: {} }),
    checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }),
  },
  settings: { getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }), getSettings: vi.fn().mockResolvedValue({}) },
  tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) },
} }))
vi.mock('@/api/admin/accounts', () => ({ getAntigravityDefaultModelMapping: vi.fn().mockResolvedValue([]) }))
// Model whitelist behavior has its own suite; isolate account group/tier saves.
vi.mock('@/components/account/ModelWhitelistSelector.vue', () => ({ default: { name: 'ModelWhitelistSelector', template: '<div />' } }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key }),
}))

import CreateAccountModal from '../CreateAccountModal.vue'
import EditAccountModal from '../EditAccountModal.vue'

const BaseDialogStub = defineComponent({ props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' })
const GroupSelectorStub = defineComponent({ props: ['modelValue'], emits: ['update:modelValue'], template: '<div />' })
const stubs = {
  BaseDialog: BaseDialogStub, GroupSelector: GroupSelectorStub,
  OAuthAuthorizationFlow: true, ConfirmDialog: true, Select: true, Icon: true,
  PlatformIcon: true, ProxySelector: true, ProxyAdBanner: true,
  ModelWhitelistSelector: true, QuotaLimitCard: true,
}
const initialGroups = [
  { id: 1, request_speed_selection_enabled: false, name: 'Ordinary OpenAI', platform: 'openai', status: 'active', subscription_type: 'standard', rate_multiplier: 1 },
  { id: 2, request_speed_selection_enabled: true, default_request_speed_tier: 'ultrafast', name: 'Enabled Ultra', platform: 'openai', status: 'active', subscription_type: 'standard', rate_multiplier: 1 },
  { id: 3, name: 'Unconfigured OpenAI', platform: 'openai', status: 'active', subscription_type: 'standard', rate_multiplier: 1 },
  { id: 4, request_speed_selection_enabled: true, default_request_speed_tier: 'ultrafast', name: 'Other enabled group', platform: 'openai', status: 'active', subscription_type: 'standard', rate_multiplier: 1 },
] as AdminGroup[]
let groups: AdminGroup[]
let mappings: Record<string, Array<'fast' | 'ultrafast'>>
const account = {
  id: 42, name: 'Local account', notes: '', platform: 'openai', type: 'apikey',
  credentials: { api_key: 'fixture-only', base_url: 'https://preview.invalid', model_mapping: {} },
  extra: {}, proxy_id: null, concurrency: 1, priority: 1, rate_multiplier: 1,
  status: 'active', group_ids: [], expires_at: null, auto_pause_on_expired: false,
} as Account
const mounted: VueWrapper[] = []

async function selectGroups(wrapper: VueWrapper, ids: number[]) {
  wrapper.getComponent(GroupSelectorStub).vm.$emit('update:modelValue', ids)
  await flushPromises()
}
async function open(mode: 'create' | 'edit', ids: number[]) {
  const wrapper = mode === 'create'
    ? mount(CreateAccountModal, { props: { show: false, proxies: [], groups }, global: { stubs } })
    : mount(EditAccountModal, { props: { show: false, proxies: [], groups, account: { ...account, group_ids: ids, request_speed_tiers_by_group: mappings } }, global: { stubs } })
  mounted.push(wrapper)
  await wrapper.setProps({ show: true })
  if (mode === 'create') {
    await wrapper.findAll('button').find(button => button.text() === 'OpenAI')!.trigger('click')
    await wrapper.findAll('button').find(button => button.text().includes('API Key'))!.trigger('click')
    await selectGroups(wrapper, ids)
  }
  await flushPromises()
  return wrapper
}
async function submit(wrapper: VueWrapper, mode: 'create' | 'edit') {
  if (mode === 'create') {
    await wrapper.get('form#create-account-form input[type="text"]').setValue('Local account')
    await wrapper.get('form#create-account-form input[type="password"]').setValue('fixture-only')
  }
  await wrapper.get(`form#${mode}-account-form`).trigger('submit.prevent')
  await flushPromises()
}
beforeEach(() => {
  vi.clearAllMocks()
  groups = initialGroups.map(group => ({ ...group }))
  mappings = {}
  mocks.create.mockResolvedValue(account)
  mocks.update.mockResolvedValue(account)
})
afterEach(() => mounted.splice(0).forEach(wrapper => wrapper.unmount()))

describe.each(['create', 'edit'] as const)('%s account request-speed group gate', mode => {
  it.each([{ ids: [1] }, { ids: [3] }, { ids: [1, 3] }])('hides the whole section for ordinary groups $ids', async ({ ids }) => {
    const wrapper = await open(mode, ids)
    expect(wrapper.find('[data-testid="account-request-speed-fields"]').exists()).toBe(false)
    await submit(wrapper, mode)
    expect(mode === 'create' ? mocks.create : mocks.update).toHaveBeenCalledOnce()
    const nativeSave = mode === 'create' ? mocks.create : mocks.update
    const payload = nativeSave.mock.calls[0][mode === 'create' ? 0 : 1]
    expect(payload).not.toHaveProperty('request_speed_tiers_by_group')
  })

  it('shows only enabled selected groups and hides immediately when they are deselected', async () => {
    const wrapper = await open(mode, [1, 2, 3])
    expect(wrapper.find('[data-testid="account-group-speed-2-fast"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="account-group-speed-1-fast"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="account-group-speed-3-fast"]').exists()).toBe(false)
    await wrapper.get('[data-testid="account-group-speed-2-fast"]').setValue(true)
    await selectGroups(wrapper, [1, 3])
    expect(wrapper.find('[data-testid="account-request-speed-fields"]').exists()).toBe(false)
    await submit(wrapper, mode)
    const nativeSave = mode === 'create' ? mocks.create : mocks.update
    const payload = nativeSave.mock.calls[0][mode === 'create' ? 0 : 1]
    expect(payload).not.toHaveProperty('request_speed_tiers_by_group')
  })
})

describe('native atomic request-speed payload', () => {
  it.each(['create', 'edit'] as const)('%s saves the selected enabled-group mapping in its native request', async mode => {
    const wrapper = await open(mode, [1, 2, 3])
    await wrapper.get('[data-testid="account-group-speed-2-fast"]').setValue(true)
    await submit(wrapper, mode)
    const nativeSave = mode === 'create' ? mocks.create : mocks.update
    expect(nativeSave).toHaveBeenCalledOnce()
    const payload = nativeSave.mock.calls[0][mode === 'create' ? 0 : 1]
    expect(payload.group_ids).toEqual([1, 2, 3])
    expect(payload.request_speed_tiers_by_group).toEqual({ '2': ['fast'] })
  })

  it('does not resubmit a hidden disabled mapping and preserves the other enabled mapping', async () => {
    groups.find(group => group.id === 2)!.request_speed_selection_enabled = false
    mappings = { '2': ['fast'], '4': ['ultrafast'] }
    const wrapper = await open('edit', [1, 2, 4])
    expect(wrapper.find('[data-testid="account-group-speed-2-fast"]').exists()).toBe(false)
    await submit(wrapper, 'edit')
    expect(mocks.update).toHaveBeenCalledOnce()
    expect(mocks.update.mock.calls[0][1].request_speed_tiers_by_group).toEqual({ '4': ['ultrafast'] })
  })

  it('preserves other selected enabled mappings when changing one group', async () => {
    mappings = { '2': ['ultrafast'], '4': ['ultrafast'] }
    const wrapper = await open('edit', [1, 2, 4])
    await wrapper.get('[data-testid="account-group-speed-2-fast"]').setValue(true)
    await submit(wrapper, 'edit')
    expect(mocks.update).toHaveBeenCalledOnce()
    expect(mocks.update.mock.calls[0][1].request_speed_tiers_by_group).toEqual({ '2': ['fast'], '4': ['ultrafast'] })
  })

  it.each(['create', 'edit'] as const)('%s uses mutually exclusive fixed-tier radios with an unassigned option', async mode => {
    const wrapper = await open(mode, [2, 4])
    const fast = wrapper.get('[data-testid="account-group-speed-2-fast"]')
    const ultra = wrapper.get('[data-testid="account-group-speed-2-ultrafast"]')
    const unassigned = wrapper.get('[data-testid="account-group-speed-2-unassigned"]')
    expect(fast.attributes('type')).toBe('radio')
    expect(ultra.attributes('type')).toBe('radio')
    expect(unassigned.element).toHaveProperty('checked', true)
    await fast.setValue(true)
    await ultra.setValue(true)
    expect(fast.element).toHaveProperty('checked', false)
    expect(ultra.element).toHaveProperty('checked', true)
    await wrapper.get('[data-testid="account-group-speed-4-fast"]').setValue(true)
    expect(ultra.element).toHaveProperty('checked', true)
    await submit(wrapper, mode)
    const nativeSave = mode === 'create' ? mocks.create : mocks.update
    expect(nativeSave.mock.calls[0][mode === 'create' ? 0 : 1].request_speed_tiers_by_group).toEqual({ '2': ['ultrafast'], '4': ['fast'] })
  })

  it('clears the chosen tier via unassigned without clearing another group', async () => {
    mappings = { '2': ['fast'], '4': ['ultrafast'] }
    const wrapper = await open('edit', [2, 4])
    await wrapper.get('[data-testid="account-group-speed-2-unassigned"]').setValue(true)
    expect(wrapper.get('[data-testid="account-group-speed-2-fast"]').element).toHaveProperty('checked', false)
    await submit(wrapper, 'edit')
    expect(mocks.update.mock.calls[0][1].request_speed_tiers_by_group).toEqual({ '4': ['ultrafast'] })
  })

  it('clears only the unlinked mapping and does not assign the ordinary replacement group', async () => {
    mappings = { '2': ['fast'], '4': ['ultrafast'] }
    const wrapper = await open('edit', [2, 4])
    await selectGroups(wrapper, [1, 4])
    await submit(wrapper, 'edit')
    expect(mocks.update.mock.calls[0][1].request_speed_tiers_by_group).toEqual({ '4': ['ultrafast'] })
    expect(mocks.update.mock.calls[0][1].group_ids).toEqual([1, 4])
  })

  it('sends an empty mapping when the last enabled group is unlinked', async () => {
    mappings = { '2': ['fast'] }
    const wrapper = await open('edit', [2])
    await selectGroups(wrapper, [1])
    await submit(wrapper, 'edit')
    expect(mocks.update).toHaveBeenCalledOnce()
    expect(mocks.update.mock.calls[0][1].request_speed_tiers_by_group).toEqual({})
    expect(mocks.update.mock.calls[0][1].group_ids).toEqual([1])
  })

  it.each(['create', 'edit'] as const)('%s leaves the form and selection intact when native save rejects', async mode => {
    const nativeSave = mode === 'create' ? mocks.create : mocks.update
    nativeSave.mockRejectedValueOnce({ reason: 'REQUEST_SPEED_DISABLED', message: 'The group no longer accepts Fast' })
    const wrapper = await open(mode, [1, 2])
    await wrapper.get('[data-testid="account-group-speed-2-fast"]').setValue(true)
    await submit(wrapper, mode)
    expect(nativeSave).toHaveBeenCalledOnce()
    expect(wrapper.emitted('close')).toBeUndefined()
    expect(wrapper.emitted(mode === 'create' ? 'created' : 'updated')).toBeUndefined()
    expect(wrapper.get('[data-testid="account-group-speed-2-fast"]').element).toHaveProperty('checked', true)
    expect(mocks.showError).toHaveBeenCalled()
    expect(wrapper.get('[data-testid="account-request-speed-error"]').text()).toContain('The group no longer accepts Fast')
    await submit(wrapper, mode)
    expect(nativeSave).toHaveBeenCalledTimes(2)
    expect(nativeSave.mock.calls[1][mode === 'create' ? 0 : 1].request_speed_tiers_by_group).toEqual({ '2': ['fast'] })
  })
})
