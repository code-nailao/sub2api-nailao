import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import ChannelCardBoard from '../ChannelCardBoard.vue'
import ChannelCard from '../ChannelCard.vue'
import DisplayOrderDialog from '../DisplayOrderDialog.vue'
import { row } from './fixtures'

const mocks = vi.hoisted(() => ({ available: vi.fn(), rates: vi.fn(), getConfig: vi.fn(), updateConfig: vi.fn(), success: vi.fn() }))
vi.mock('vue-i18n', async (importOriginal) => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key, locale: { value: 'en' } }) }))
vi.mock('@/api/groups', () => ({ default: { getAvailable: mocks.available, getUserGroupRates: mocks.rates } }))
vi.mock('@/api/channelMonitorV2', () => ({ getConfig: mocks.getConfig, updateConfig: mocks.updateConfig }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess: mocks.success }) }))
const wrappers: ReturnType<typeof mount>[] = []
function render(isAdmin = false) {
  const wrapper = mount(ChannelCardBoard, {
    props: { rows: [row(1)], config: null, coverage: null, groups: [{ id: 1, name: 'A', platform: 'openai' }], isAdmin, loading: false },
    global: { stubs: { ChannelCard: true, DisplayOrderDialog: true } },
  })
  wrappers.push(wrapper)
  return wrapper
}
beforeEach(() => {
  vi.clearAllMocks()
  mocks.available.mockResolvedValue([{ id: 1, name: 'A', platform: 'openai', rate_multiplier: .065 }])
  mocks.rates.mockResolvedValue({ 1: 0 })
})
afterEach(() => wrappers.splice(0).forEach(w => w.unmount()))

describe('channel cards authorization and rates', () => {
  it('uses a current user override of zero and does not expose admin ordering controls', async () => {
    const wrapper = render()
    await flushPromises()
    expect(wrapper.getComponent(ChannelCard).props('multiplier')).toBe(0)
    expect(wrapper.findAll('button').some(b => b.text().includes('cards.sortTitle'))).toBe(false)
    expect(mocks.getConfig).not.toHaveBeenCalled()
    expect(mocks.updateConfig).not.toHaveBeenCalled()
    expect(wrapper.text()).not.toContain('channelMonitorV2.cards.metricHint')
    expect(wrapper.find('.legend-dot').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('channelMonitorV2.cards.attentionOnly')
    expect(wrapper.find('input[type="checkbox"]').exists()).toBe(false)
  })

  it('leaves rates blank if the authoritative user-rate request fails', async () => {
    mocks.rates.mockRejectedValueOnce(new Error('unavailable'))
    const wrapper = render()
    await flushPromises()
    expect(wrapper.getComponent(ChannelCard).props('multiplier')).toBeUndefined()
    expect(wrapper.text()).toContain('channelMonitorV2.cards.ratesUnavailable')
  })

  it('shows the populated image category by default and removes it when provider mode is selected', async () => {
    const wrapper = render()
    await wrapper.setProps({ rows: [row(1, 'Codex'), row(2, 'GPT Image 2')] })
    expect(wrapper.find('#sources-monitor-image').exists()).toBe(true)
    await wrapper.findAll('.category-tab').find(button => button.text().includes('channelMonitorV2.cards.categories.image'))!.trigger('click')
    expect(wrapper.findAllComponents(ChannelCard)).toHaveLength(1)
    await wrapper.setProps({ config: { display_order: { image_grouping: 'provider' } } })
    expect(wrapper.find('#sources-monitor-image').exists()).toBe(false)
    expect(wrapper.findAllComponents(ChannelCard).map(card => card.props('category'))).toEqual(['openai', 'openai'])
  })

  it('offers attention filtering only to administrators and clears it after a role change', async () => {
    const wrapper = render(true)
    const warning = row(2, 'Warning')
    warning.health.overall = 'warning'
    await wrapper.setProps({ rows: [row(1), warning] })
    await wrapper.get('input[type="checkbox"]').setValue(true)
    expect(wrapper.findAllComponents(ChannelCard)).toHaveLength(1)
    await wrapper.setProps({ isAdmin: false })
    expect(wrapper.find('input[type="checkbox"]').exists()).toBe(false)
    expect(wrapper.findAllComponents(ChannelCard)).toHaveLength(2)
    await wrapper.setProps({ isAdmin: true })
    expect((wrapper.get('input[type="checkbox"]').element as HTMLInputElement).checked).toBe(false)
  })

  it('persists only the confirmed order with the loaded config version and publishes the returned config', async () => {
    const config = { version: 7, enabled: true, refresh_interval_seconds: 300, group_ids: [1, 90], platforms: [], health_thresholds: {}, display_order: { groups: { openai: [1, 90] } } }
    const order = { categories: ['domestic', 'openai'], groups: { openai: [1, 90] }, image_grouping: 'provider' }
    const saved = { ...config, version: 8, display_order: order }
    mocks.getConfig.mockResolvedValue(config)
    mocks.updateConfig.mockResolvedValue(saved)
    const wrapper = render(true)
    expect(wrapper.text()).toContain('channelMonitorV2.cards.metricHint')
    expect(wrapper.find('.legend-dot').exists()).toBe(false)
    await wrapper.findAll('button').find(b => b.text().includes('cards.sortTitle'))!.trigger('click')
    await flushPromises()
    expect(mocks.updateConfig).not.toHaveBeenCalled()
    wrapper.getComponent(DisplayOrderDialog).vm.$emit('save', order)
    await flushPromises()
    expect(mocks.updateConfig).toHaveBeenCalledWith({ ...config, display_order: order })
    expect(wrapper.emitted('saved')?.[0]?.[0]).toEqual(saved)
    expect(wrapper.getComponent(DisplayOrderDialog).props('show')).toBe(false)
  })

  it('keeps the sorting dialog open and reports a version conflict without overwriting the saved order', async () => {
    mocks.getConfig.mockResolvedValue({ version: 3, display_order: {} })
    mocks.updateConfig.mockRejectedValue(new Error('config version conflict'))
    const wrapper = render(true)
    await wrapper.findAll('button').find(b => b.text().includes('cards.sortTitle'))!.trigger('click')
    await flushPromises()
    wrapper.getComponent(DisplayOrderDialog).vm.$emit('save', { categories: ['grok'] })
    await flushPromises()
    expect(wrapper.emitted('saved')).toBeUndefined()
    expect(wrapper.getComponent(DisplayOrderDialog).props('show')).toBe(true)
    expect(wrapper.getComponent(DisplayOrderDialog).props('error')).toBeTruthy()
    expect(mocks.getConfig).toHaveBeenCalledTimes(1)
  })

  it('marks the sorting editor unavailable when its authoritative config request fails', async () => {
    mocks.getConfig.mockRejectedValueOnce(new Error('configuration unavailable'))
    const wrapper = render(true)
    await wrapper.findAll('button').find(button => button.text().includes('cards.sortTitle'))!.trigger('click')
    await flushPromises()
    const dialog = wrapper.getComponent(DisplayOrderDialog)
    expect(dialog.props('ready')).toBe(false)
    expect(dialog.props('error')).toBeTruthy()
    expect(mocks.updateConfig).not.toHaveBeenCalled()
  })

  it('invalidates a pending administrator configuration response after a role change', async () => {
    let resolveConfig!: (value: unknown) => void
    mocks.getConfig.mockImplementationOnce(() => new Promise(resolve => { resolveConfig = resolve }))
    const wrapper = render(true)
    await wrapper.findAll('button').find(button => button.text().includes('cards.sortTitle'))!.trigger('click')
    await wrapper.setProps({ isAdmin: false })
    resolveConfig({ version: 7, display_order: { groups: { openai: [90] } } })
    await flushPromises()
    await wrapper.setProps({ isAdmin: true })
    const dialog = wrapper.getComponent(DisplayOrderDialog)
    expect(dialog.props('show')).toBe(false)
    expect(dialog.props('ready')).toBe(false)
    expect(dialog.props('order')).toBeUndefined()
  })
})
