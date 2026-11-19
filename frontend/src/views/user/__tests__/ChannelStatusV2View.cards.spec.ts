import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { reactive, ref } from 'vue'
import ChannelStatusV2View from '../ChannelStatusV2View.vue'
import ChannelCardBoard from '@/features/sources/monitor-v2-cards/ChannelCardBoard.vue'
import { metric, health, row } from '@/features/sources/monitor-v2-cards/__tests__/fixtures'
import type { MonitorConfig, MonitorSnapshot } from '@/api/channelMonitorV2'

const mocks = vi.hoisted(() => ({ snapshot: vi.fn(), matrix: vi.fn(), dimensions: vi.fn(), models: vi.fn(), errors: vi.fn(), users: vi.fn(), replace: vi.fn(), showError: vi.fn(), auth: { isAdmin: true }, query: {} as Record<string, string> }))
vi.mock('vue-i18n', async (importOriginal) => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key, te: () => true, locale: ref('en') }) }))
vi.mock('vue-router', () => ({ useRoute: () => ({ query: mocks.query }), useRouter: () => ({ replace: mocks.replace }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => mocks.auth }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: mocks.showError }) }))
vi.mock('@/utils/featureFlags', () => ({ isChannelMonitorThroughputHidden: () => false, isChannelMonitorUserRankingHidden: () => false }))
vi.mock('@/api/channelMonitorV2', () => ({ getSnapshot: mocks.snapshot, getMatrix: mocks.matrix, getDimensions: mocks.dimensions, getModels: mocks.models, getErrors: mocks.errors, getUsers: mocks.users }))
const wrappers: ReturnType<typeof mount>[] = []
const config: MonitorConfig = { version: 7, enabled: true, refresh_interval_seconds: 300, platforms: [], group_ids: [], health_thresholds: { minimum_sample: 50, warning_error_rate: .05, critical_error_rate: .2, target_ttft_ms: 3000, warning_ttft_ms: 5000, critical_ttft_ms: 10000, warning_cache_rate: .85, critical_cache_rate: .6, error_weight: .6, ttft_weight: .2, cache_weight: .2 } }
const coverage = { requested_start: '2026-11-17T00:00:00Z', requested_end: '2026-11-17T01:30:00Z', coverage_start: '2026-11-17T00:00:00Z', data_through: '2026-11-17T01:30:00Z', computed_at: '2026-11-17T01:31:00Z', aggregation_lag_seconds: 0, coverage_complete: true, bucket_seconds: 300 }
function snapshot(): MonitorSnapshot { return { config: { ...config }, coverage, metrics: metric(), health: health(), trend: [] } }
beforeEach(() => {
  vi.clearAllMocks()
  mocks.auth = reactive({ isAdmin: true })
  mocks.query = { group_by: 'platform_model' }
  mocks.snapshot.mockResolvedValue(snapshot())
  mocks.matrix.mockResolvedValue({ coverage, group_by: 'platform_group', items: [row(1)] })
  mocks.dimensions.mockResolvedValue({ platforms: [], models: [], groups: [{ id: 1, name: 'A', platform: 'openai', request_count: 0 }] })
  mocks.models.mockResolvedValue({ items: [] })
  mocks.errors.mockResolvedValue({ items: [] })
  mocks.users.mockResolvedValue({ items: [] })
})
afterEach(() => wrappers.splice(0).forEach(w => w.unmount()))
function render() {
  const wrapper = mount(ChannelStatusV2View, { global: { stubs: {
    AppLayout: { template: '<div><slot /></div>' }, ChannelCardBoard: true,
    MonitorTrendChart: true, RelayPulseMatrix: true, FilterMultiSelect: true, Select: true,
  } } })
  wrappers.push(wrapper)
  return wrapper
}
describe('V2 card view integration', () => {
  it('defaults to cards and requests group rows despite a persisted matrix-only dimension', async () => {
    const wrapper = render()
    await flushPromises()
    expect(wrapper.findComponent(ChannelCardBoard).exists()).toBe(true)
    expect(mocks.matrix.mock.calls[0][1]).toBe('platform_group')
    expect(mocks.models).not.toHaveBeenCalled()
    expect(wrapper.findAll('button').some(b => b.text() === 'channelMonitorV2.healthMode.overall')).toBe(false)
  })

  it('keeps a saved display order when an older poll finishes after the save', async () => {
    const wrapper = render()
    await flushPromises()
    let resolveOld!: (value: MonitorSnapshot) => void
    mocks.snapshot.mockImplementationOnce(() => new Promise<MonitorSnapshot>(resolve => { resolveOld = resolve }))
    await wrapper.get('button[title="common.refresh"]').trigger('click')
    const saved = { ...config, version: 8, display_order: { categories: ['domestic', 'openai'] as const } }
    wrapper.getComponent(ChannelCardBoard).vm.$emit('saved', saved)
    resolveOld(snapshot())
    await flushPromises()
    expect(wrapper.getComponent(ChannelCardBoard).props('config').version).toBe(8)
    expect(wrapper.getComponent(ChannelCardBoard).props('config').display_order.categories).toEqual(['domestic', 'openai'])
  })

  it('shows only cards to ordinary users and ignores diagnostic URL parameters without requesting detail APIs', async () => {
    mocks.auth.isAdmin = false
    mocks.query = { trend_view: 'line', group_by: 'platform_model', platform: 'openai', group: '1', model: 'gpt-6', tab: 'users', range: '7d' }
    const wrapper = render()
    await flushPromises()
    expect(wrapper.findComponent(ChannelCardBoard).exists()).toBe(true)
    expect(wrapper.find('#monitor-admin-diagnostics').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('channelMonitorV2.cards.adminDiagnostics')
    expect(wrapper.text()).not.toContain('channelMonitorV2.cards.advancedDetails')
    expect(wrapper.text()).not.toContain('channelMonitorV2.trendView.line')
    expect(mocks.matrix.mock.calls[0][0]).toEqual({ range: '7d', platforms: [], groupIds: [], models: [] })
    expect(mocks.matrix.mock.calls[0][1]).toBe('platform_group')
    expect(mocks.dimensions).not.toHaveBeenCalled()
    expect(mocks.models).not.toHaveBeenCalled()
    expect(mocks.errors).not.toHaveBeenCalled()
    expect(mocks.users).not.toHaveBeenCalled()
  })

  it('retains admin filters and matrix view inside an explicit collapsible diagnostic area', async () => {
    const wrapper = render()
    await flushPromises()
    expect(wrapper.find('#monitor-admin-diagnostics').exists()).toBe(false)
    await wrapper.get('button[aria-controls="monitor-admin-diagnostics"]').trigger('click')
    expect(wrapper.find('#monitor-admin-diagnostics').exists()).toBe(true)
    await wrapper.findAll('button').find(b => b.text() === 'channelMonitorV2.trendView.pulse')!.trigger('click')
    await flushPromises()
    expect(wrapper.find('relay-pulse-matrix-stub').exists()).toBe(true)
    expect(mocks.models).toHaveBeenCalled()
    await wrapper.get('button[aria-controls="monitor-admin-diagnostics"]').trigger('click')
    await flushPromises()
    expect(wrapper.find('#monitor-admin-diagnostics').exists()).toBe(false)
    expect(wrapper.findComponent(ChannelCardBoard).exists()).toBe(true)
  })

  it('identifies retained data after a failed range change and clears the warning only after a successful retry', async () => {
    const wrapper = render()
    await flushPromises()
    const previousCoverage = wrapper.getComponent(ChannelCardBoard).props('coverage')
    mocks.snapshot.mockRejectedValueOnce(new Error('statistics unavailable'))
    await wrapper.findAll('button').find(button => button.text() === 'channelMonitorV2.ranges.24h')!.trigger('click')
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('channelMonitorV2.cards.loadFailedRetained')
    expect(wrapper.getComponent(ChannelCardBoard).props('coverage')).toEqual(previousCoverage)
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(mocks.snapshot.mock.calls.at(-1)?.[0].range).toBe('24h')
  })

  it('shows an initial statistics failure instead of presenting it as an empty channel list', async () => {
    mocks.snapshot.mockRejectedValueOnce(new Error('offline'))
    const wrapper = render()
    await flushPromises()
    expect(wrapper.get('[role="alert"]').text()).toContain('channelMonitorV2.cards.loadFailedRetry')
    expect(wrapper.findComponent(ChannelCardBoard).exists()).toBe(false)
  })

  it('clears administrator-scoped data immediately after a role change and rejects a late administrator poll', async () => {
    const privateRow = row(90, 'Admin-only channel')
    mocks.matrix.mockResolvedValue({ coverage, group_by: 'platform_group', items: [privateRow] })
    const wrapper = render()
    await flushPromises()
    expect(wrapper.getComponent(ChannelCardBoard).props('rows')).toEqual([privateRow])
    let resolveSnapshot!: (value: MonitorSnapshot) => void
    let resolveMatrix!: (value: unknown) => void
    mocks.snapshot.mockImplementationOnce(() => new Promise<MonitorSnapshot>(resolve => { resolveSnapshot = resolve }))
    mocks.matrix.mockImplementationOnce(() => new Promise(resolve => { resolveMatrix = resolve })).mockRejectedValueOnce(new Error('public scope unavailable'))
    await wrapper.get('button[title="common.refresh"]').trigger('click')
    const adminSignal = mocks.snapshot.mock.calls.at(-1)?.[2] as AbortSignal
    mocks.auth.isAdmin = false
    await flushPromises()
    expect(adminSignal.aborted).toBe(true)
    expect(wrapper.findComponent(ChannelCardBoard).exists()).toBe(false)
    resolveSnapshot(snapshot())
    resolveMatrix({ coverage, group_by: 'platform_group', items: [privateRow] })
    await flushPromises()
    expect(wrapper.findComponent(ChannelCardBoard).exists()).toBe(false)
    expect(mocks.matrix.mock.calls.at(-1)?.[2]).toBe(false)
    mocks.matrix.mockResolvedValue({ coverage, group_by: 'platform_group', items: [row(2, 'Public channel')] })
    await wrapper.get('[role="alert"] button').trigger('click')
    await flushPromises()
    expect(wrapper.getComponent(ChannelCardBoard).props('rows').map((value: { group_id: number }) => value.group_id)).toEqual([2])
    expect(wrapper.getComponent(ChannelCardBoard).props('groups')).toEqual([])
  })
})
