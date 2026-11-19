import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { reactive } from 'vue'
import CustomPageView from '../CustomPageView.vue'

const fixtures = vi.hoisted(() => ({
  app: { publicSettingsLoaded: true, cachedPublicSettings: { custom_menu_items: [] as { id: string; url: string; label: string }[] } },
  refresh: vi.fn().mockResolvedValue({}),
}))
const route = reactive({ params: { id: 'canvas' }, path: '/custom/canvas', query: { studio_view: 'embedded' } })
vi.mock('vue-router', () => ({ useRoute: () => route }))
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
  vi.unstubAllGlobals(); vi.useRealTimers(); vi.clearAllMocks()
})
function setup(page = 'canvas', mode = 'embedded') {
  route.params.id = page; route.path = `/custom/${page}`; route.query.studio_view = mode
  fixtures.app.cachedPublicSettings.custom_menu_items = [
    { id: 'canvas', label: '无限画布', url: window.location.origin + '/canvas?token=stale&user_id=9&src_url=private&src_host=private' },
    { id: 'images', label: '图片工作台', url: window.location.origin + '/studio' },
  ]
  vi.useFakeTimers(); vi.stubGlobal('ResizeObserver', class { observe() {} disconnect() {} })
  const wrapper = mount(CustomPageView, { attachTo: document.body, global: { stubs: { Icon: true } } })
  wrappers.push(wrapper)
  return { wrapper, frame: wrapper.get('iframe').element as HTMLIFrameElement }
}
describe('Canvas managed custom-page shell', () => {
  it('has no generic card or credential-bearing open link from first paint', () => {
    const { wrapper, frame } = setup()
    expect(wrapper.attributes('data-mode')).toBe('studio')
    expect(wrapper.find('.card').exists()).toBe(false)
    expect(wrapper.find('.custom-open-fab').exists()).toBe(false)
    expect(wrapper.get('.studio-loading-state').text()).toBe('正在打开无限画布')
    expect(new URL(frame.src).search).toBe('?ui_mode=embedded')
    expect(frame.title).toBe('无限画布')
  })
  it('supports standalone canvas with a clean Main return URL', async () => {
    const { wrapper, frame } = setup('canvas', 'standalone')
    expect(wrapper.attributes('data-mode')).toBe('studio-standalone')
    frame.dispatchEvent(new Event('load'))
    vi.advanceTimersByTime(20000); await flushPromises()
    expect(wrapper.get('.custom-open-fab').attributes('href')).toBe(window.location.origin + '/custom/canvas?studio_view=standalone')
    expect(wrapper.get('iframe').element).toBe(frame)
  })
  it('remounts only on custom-page identity change and rejects the old Studio source', async () => {
    const { wrapper, frame: oldFrame } = setup('images')
    const oldSource = oldFrame.contentWindow
    oldFrame.dispatchEvent(new Event('load'))
    route.params.id = 'canvas'; route.path = '/custom/canvas'
    await flushPromises()
    const canvas = wrapper.get('iframe').element as HTMLIFrameElement
    expect(canvas).not.toBe(oldFrame)
    expect(oldFrame.isConnected).toBe(false)
    expect(wrapper.get('.studio-loading-state').text()).toBe('正在打开无限画布')
    const posts = vi.spyOn(canvas.contentWindow!, 'postMessage')
    canvas.dispatchEvent(new Event('load')); await flushPromises()
    const config = posts.mock.calls.at(-1)![0]
    window.dispatchEvent(new MessageEvent('message', { origin: window.location.origin, source: oldSource,
      data: { ...config, type: 'sources:studio-host-config-applied', integrated: true } }))
    await flushPromises()
    expect(canvas.getAttribute('aria-busy')).toBe('true')
    window.dispatchEvent(new MessageEvent('message', { origin: window.location.origin, source: canvas.contentWindow,
      data: { ...config, type: 'sources:studio-host-config-applied', integrated: true } }))
    await flushPromises(); expect(canvas.hasAttribute('aria-busy')).toBe(false)
    const source = canvas.src
    document.documentElement.classList.add('dark'); await flushPromises()
    expect(wrapper.get('iframe').element).toBe(canvas); expect(canvas.src).toBe(source)
  })
  it('keeps the existing credential-free native session check for Canvas', async () => {
    const { wrapper, frame } = setup()
    const posts = vi.spyOn(frame.contentWindow!, 'postMessage')
    window.dispatchEvent(new MessageEvent('message', { origin: window.location.origin, source: frame.contentWindow,
      data: { type: 'sub2api:session-check', requestId: 'canvas-session-check-0001' } }))
    await flushPromises()
    expect(fixtures.refresh).toHaveBeenCalledWith('/auth/me')
    expect(posts.mock.calls.some(([data]) => data.type === 'sub2api:session-checked' && data.ok)).toBe(true)
    expect(wrapper.get('iframe').element).toBe(frame)
    expect(JSON.stringify(posts.mock.calls)).not.toContain('must-not-travel-in-url')
  })
})
