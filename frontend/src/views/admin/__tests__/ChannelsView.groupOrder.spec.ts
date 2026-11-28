import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import ChannelsView from '@/views/admin/ChannelsView.vue'

const { listChannels, getGroups, getWebSearchEmulationConfig, showSuccess, showError } = vi.hoisted(() => ({
  listChannels: vi.fn(),
  getGroups: vi.fn(),
  getWebSearchEmulationConfig: vi.fn(),
  showSuccess: vi.fn(),
  showError: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    channels: { list: listChannels },
    groups: { getAll: getGroups },
    settings: { getWebSearchEmulationConfig }
  }
}))

vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showSuccess, showError }) }))

vi.mock('vue-i18n', async () => ({
  ...(await vi.importActual<typeof import('vue-i18n')>('vue-i18n')),
  useI18n: () => ({ t: (key: string, fallback?: string) => fallback ?? key })
}))

const clearSort = vi.fn()

// DataTable stub that exposes clearSort (so the view's ref call is observable) and can emit sort.
const DataTableStub = defineComponent({
  name: 'DataTable',
  props: ['columns', 'data', 'loading', 'serverSideSort'],
  emits: ['sort'],
  setup(_props, { expose }) {
    expose({ clearSort })
    return () => h('div', { class: 'data-table-stub' })
  }
})

// Pagination stub that lets the test navigate to a non-first page.
const PaginationStub = defineComponent({
  name: 'Pagination',
  props: ['page', 'total', 'pageSize'],
  emits: ['update:page', 'update:pageSize'],
  setup(_props, { emit }) {
    return () => h('button', { class: 'goto-page-3', onClick: () => emit('update:page', 3) }, 'next')
  }
})

function mountView() {
  return mount(ChannelsView, {
    global: {
      stubs: {
        AppLayout: defineComponent({ template: '<main><slot /></main>' }),
        TablePageLayout: defineComponent({
          template:
            '<section><slot name="filters" /><slot name="table" /><slot name="pagination" /></section>'
        }),
        DataTable: DataTableStub,
        Pagination: PaginationStub,
        BaseDialog: true,
        ConfirmDialog: true,
        EmptyState: true,
        Select: true,
        Icon: true,
        PlatformIcon: true,
        Toggle: true
      }
    }
  })
}

function lastListFilters() {
  const call = listChannels.mock.calls.at(-1)!
  return { page: call[0], filters: call[2] }
}

describe('ChannelsView group-order default', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    localStorage.clear()
    listChannels.mockResolvedValue({ items: [{ id: 1 }, { id: 2 }], total: 2 })
    getGroups.mockResolvedValue([])
    getWebSearchEmulationConfig.mockResolvedValue({ enabled: false })
  })

  it('requests sort_by=group_order, sort_order=asc on first load', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(listChannels).toHaveBeenCalled()
    const { page, filters } = lastListFilters()
    expect(page).toBe(1)
    expect(filters).toMatchObject({ sort_by: 'group_order', sort_order: 'asc' })

    wrapper.unmount()
  })

  it('switches to a column sort, then restores group order on the toolbar button', async () => {
    const wrapper = mountView()
    await flushPromises()

    // Column header sort → request carries that column.
    wrapper.findComponent(DataTableStub).vm.$emit('sort', 'name', 'asc')
    await flushPromises()
    expect(lastListFilters().filters).toMatchObject({ sort_by: 'name', sort_order: 'asc' })

    // Navigate away from page 1 so the restore can be observed resetting it.
    await wrapper.get('.goto-page-3').trigger('click')
    await flushPromises()
    expect(lastListFilters().page).toBe(3)

    clearSort.mockClear()

    // Click the "按分组顺序" button.
    const button = wrapper.findAll('button').find(b => b.text() === 'Group Order')
    expect(button).toBeDefined()
    await button!.trigger('click')
    await flushPromises()

    const { page, filters } = lastListFilters()
    expect(page).toBe(1)
    expect(filters).toMatchObject({ sort_by: 'group_order', sort_order: 'asc' })
    expect(clearSort).toHaveBeenCalledTimes(1)

    wrapper.unmount()
  })
})
