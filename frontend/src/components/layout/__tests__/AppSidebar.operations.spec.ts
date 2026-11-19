import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, RouterLinkStub } from '@vue/test-utils'
import AppSidebar from '../AppSidebar.vue'

const state = vi.hoisted(() => ({
  route: { path: '/admin/dashboard' },
  app: {
    sidebarCollapsed: false, mobileOpen: false, sidebarScrollTop: 0,
    siteName: 'Sources', siteLogo: '', siteVersion: '', publicSettingsLoaded: true,
    backendModeEnabled: false, cachedPublicSettings: { custom_menu_items: [] },
    toggleSidebar: vi.fn(), setMobileOpen: vi.fn(),
  },
  auth: { isAdmin: true, isSimpleMode: true },
  admin: { opsMonitoringEnabled: true, paymentEnabled: false, customMenuItems: [], fetch: vi.fn() },
}))

vi.mock('@/stores', () => ({
  useAppStore: () => state.app,
  useAuthStore: () => state.auth,
  useAdminSettingsStore: () => state.admin,
  useOnboardingStore: () => ({ isCurrentStep: () => false, nextStep: vi.fn() }),
}))
vi.mock('vue-router', () => ({ useRoute: () => state.route, useRouter: () => ({ push: vi.fn() }) }))
vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))
vi.mock('@/features/sources/useAppearance', () => ({ useAppearance: () => ({ isDark: false, toggleTheme: vi.fn() }) }))
vi.mock('@/composables/useBatchImageAccess', () => ({ useBatchImageAccess: () => ({ canUseBatchImage: { value: false }, refreshBatchImageAccess: vi.fn() }) }))
vi.mock('@/utils/featureFlags', async (importOriginal) => ({
  ...await importOriginal<typeof import('@/utils/featureFlags')>(),
  makeSidebarFlag: () => () => false,
}))

let wrapper: ReturnType<typeof mount>
function render() {
  wrapper = mount(AppSidebar, {
    global: {
      stubs: { RouterLink: RouterLinkStub, SidebarThemePicker: true, VersionBadge: true, Icon: true },
    },
  })
  return wrapper
}
function operationButton() {
  return wrapper.findAll('button').find((button) => button.text() === 'nav.operations')
}
function imageLinks() {
  return wrapper.findAllComponents(RouterLinkStub).filter((link) => link.props('to') === '/admin/image-tasks')
}

beforeEach(() => {
  state.route.path = '/admin/dashboard'
  state.auth.isAdmin = true
  state.admin.opsMonitoringEnabled = true
})
afterEach(() => { wrapper?.unmount(); vi.clearAllMocks() })

describe('image exceptions in operations navigation', () => {
  it('keeps image handling inside the operations group rather than a separate top-level link', async () => {
    render()
    expect(imageLinks()).toHaveLength(0)
    await operationButton()!.trigger('click')
    expect(imageLinks()).toHaveLength(1)
    expect(imageLinks()[0].text()).toBe('nav.imageTaskReview')
    expect(wrapper.findAllComponents(RouterLinkStub).some((link) => link.props('to') === '/admin/ops')).toBe(true)
    await operationButton()!.trigger('click')
    expect(imageLinks()).toHaveLength(0)
  })

  it('retains access to image exceptions when ops monitoring is disabled', async () => {
    state.admin.opsMonitoringEnabled = false
    render()
    await operationButton()!.trigger('click')
    expect(imageLinks()).toHaveLength(1)
    expect(wrapper.findAllComponents(RouterLinkStub).some((link) => link.props('to') === '/admin/ops')).toBe(false)
  })

  it('opens the group for an existing image-task deep link', () => {
    state.route.path = '/admin/image-tasks'
    render()
    expect(imageLinks()).toHaveLength(1)
    expect(imageLinks()[0].classes()).toContain('sidebar-link-active')
  })

  it('does not expose the operations group or image handling to a regular user', () => {
    state.auth.isAdmin = false
    render()
    expect(operationButton()).toBeUndefined()
    expect(imageLinks()).toHaveLength(0)
  })
})
