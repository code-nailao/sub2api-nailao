import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import CustomPageView from '../CustomPageView.vue'

const fixtures = vi.hoisted(() => ({
  app: { publicSettingsLoaded: true, cachedPublicSettings: { custom_menu_items: [{ id: 'images', url: '' }] } },
  route: { params: { id: 'images' }, path: '/custom/images', query: { studio_view: 'standalone' } },
  refresh: vi.fn().mockResolvedValue({}),
}))
vi.mock('vue-router', () => ({ useRoute: () => fixtures.route }))
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key, locale: { value: 'zh' } }) }))
vi.mock('@/stores', () => ({ useAppStore: () => fixtures.app }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isAdmin: false, user: { id: 9 }, token: 'must-not-travel-in-url' }) }))
vi.mock('@/stores/adminSettings', () => ({ useAdminSettingsStore: () => ({ customMenuItems: [] }) }))
vi.mock('@/api/client', () => ({ apiClient: { get: fixtures.refresh }, buildApiUrl: (path: string) => `/api/v1${path}` }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { props: ['mode'], template: '<div :data-mode="mode"><slot /></div>' } }))

const wrappers: ReturnType<typeof mount>[] = []
afterEach(() => {
  wrappers.splice(0).forEach((wrapper) => wrapper.unmount())
  document.documentElement.classList.remove('dark')
  vi.unstubAllGlobals()
  vi.useRealTimers()
})

function setup(mode = 'standalone') {
  fixtures.route.query.studio_view = mode
  vi.useFakeTimers()
  vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} })
  fixtures.app.cachedPublicSettings.custom_menu_items[0].url = window.location.origin + '/studio'
  const wrapper = mount(CustomPageView, { attachTo: document.body, global: { stubs: { Icon: true } } })
  wrappers.push(wrapper)
  const frame = wrapper.get('iframe').element as HTMLIFrameElement
  const send = vi.spyOn(frame.contentWindow!, 'postMessage')
  return { wrapper, frame, send }
}

describe('Studio native custom page', () => {
  it('starts standalone without host chrome and keeps the same iframe and session listener', async () => {
    const { wrapper, frame, send } = setup()
    expect(frame.src).not.toContain('token=')
    expect(wrapper.find('.custom-open-fab').exists()).toBe(false)
    expect(wrapper.find('.studio-loading-state').exists()).toBe(true)
    expect(wrapper.attributes('data-mode')).toBe('studio-standalone')
    frame.dispatchEvent(new Event('load'))
    const config = send.mock.calls.at(-1)![0]
    window.dispatchEvent(new MessageEvent('message', { origin: window.location.origin, source: frame.contentWindow,
      data: { ...config, type: 'sources:studio-host-config-applied', integrated: true } }))
    await flushPromises()
    expect(wrapper.attributes('data-mode')).toBe('studio-standalone')
    expect(wrapper.get('iframe').element).toBe(frame)
    expect(wrapper.find('.custom-open-fab').exists()).toBe(false)
    window.dispatchEvent(new MessageEvent('message', { origin: window.location.origin, source: frame.contentWindow,
      data: { type: 'sub2api:session-check', requestId: 'native-session-check-0001' } }))
    await flushPromises()
    expect(fixtures.refresh).toHaveBeenCalledWith('/auth/me')
    expect(send.mock.calls.some(([data]) => data.type === 'sub2api:session-checked' && data.ok)).toBe(true)
    const oldSrc = frame.src
    document.documentElement.classList.add('dark')
    await flushPromises()
    expect(frame.src).toBe(oldSrc)
    expect(wrapper.get('iframe').element).toBe(frame)
    expect(wrapper.attributes('data-mode')).toBe('studio-standalone')
  })

  it('offers recovery when the initial iframe never loads or sends ready', async () => {
    const { wrapper, frame } = setup()
    await flushPromises()
    vi.advanceTimersByTime(20000)
    await flushPromises()
    expect(wrapper.find('.studio-loading-state').exists()).toBe(false)
    expect(wrapper.find('.studio-recovery-bar').exists()).toBe(true)
    expect(wrapper.find('.custom-open-fab').exists()).toBe(true)
    expect(wrapper.get('iframe').element).toBe(frame)
  })

  it('keeps a stable standalone shell with recovery navigation when an old Portal never acknowledges', async () => {
    const { wrapper, frame } = setup()
    frame.dispatchEvent(new Event('load'))
    vi.advanceTimersByTime(20000)
    await flushPromises()
    expect(wrapper.attributes('data-mode')).toBe('studio-standalone')
    expect(wrapper.find('.custom-open-fab').exists()).toBe(true)
    expect(wrapper.get('.custom-open-fab').attributes('href')).toBe(window.location.origin + '/custom/images?studio_view=standalone')
    expect(wrapper.find('.studio-recovery-bar a[href="/"]').exists()).toBe(true)
    expect(wrapper.get('iframe').element).toBe(frame)
    await wrapper.get('.studio-recovery-bar button').trigger('click')
    expect(wrapper.get('iframe').element).toBe(frame)
  })

  it('keeps Main navigation in embedded mode from first paint through recovery', async () => {
    const { wrapper, frame } = setup('embedded')
    expect(wrapper.attributes('data-mode')).toBe('studio')
    expect(wrapper.find('.custom-open-fab').exists()).toBe(false)
    frame.dispatchEvent(new Event('load'))
    vi.advanceTimersByTime(20000)
    await flushPromises()
    expect(wrapper.attributes('data-mode')).toBe('studio')
    expect(wrapper.find('.studio-recovery-bar').exists()).toBe(true)
    expect(wrapper.get('iframe').element).toBe(frame)
  })
})
