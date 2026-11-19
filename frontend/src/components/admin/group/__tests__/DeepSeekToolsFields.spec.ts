import { defineComponent, nextTick, ref } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import DeepSeekToolsFields from '../DeepSeekToolsFields.vue'
import type { DeepSeekToolsConfig } from '@/types'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
vi.mock('@/api/admin/accounts', () => ({ list: vi.fn(async (page: number) => ({ items: [{ id: page, name: `Tool account ${page}` }], total: 2 })) }))

function fixture(searchPrice: number | null = null, fetchPrice: number | null = null) {
  const config = ref<DeepSeekToolsConfig>({ enabled: false, auto_inject: true, account_id: null, fetch_price_per_1k: fetchPrice })
  const search = ref(searchPrice)
  const eligible = ref(true)
  const host = defineComponent({
    components: { DeepSeekToolsFields },
    setup: () => ({ config, search, eligible }),
    template: '<DeepSeekToolsFields v-model="config" v-model:search-price="search" :eligible="eligible" />'
  })
  return { wrapper: mount(host), config, search, eligible }
}

describe('DeepSeek group tool settings', () => {
  it('initializes both missing prices to 0.006 per call and loads account pages', async () => {
    const { wrapper, config, search } = fixture()
    await wrapper.find('input[type=checkbox]').setValue(true)
    await flushPromises()
    expect(search.value).toBe(6)
    expect(config.value.fetch_price_per_1k).toBe(6)
    expect(wrapper.findAll('input[type=number]').map(el => (el.element as HTMLInputElement).value)).toEqual(['0.006', '0.006'])
    expect(wrapper.findAll('option')).toHaveLength(3)
    await wrapper.find('select').setValue('2')
    expect(config.value.account_id).toBe(2)
    wrapper.unmount()
  })
  it('preserves explicitly free prices and converts edits to per-thousand units', async () => {
    const { wrapper, config, search } = fixture(0, 0)
    await wrapper.find('input[type=checkbox]').setValue(true)
    expect(search.value).toBe(0)
    expect(config.value.fetch_price_per_1k).toBe(0)
    await wrapper.findAll('input[type=number]')[1].setValue('0.007')
    expect(config.value.fetch_price_per_1k).toBe(7)
    wrapper.unmount()
  })
  it('disables tool policy when the group stops using standard billing', async () => {
    const { wrapper, config, eligible } = fixture()
    await wrapper.find('input[type=checkbox]').setValue(true)
    eligible.value = false
    await nextTick()
    expect(config.value.enabled).toBe(false)
    expect((wrapper.find('input[type=checkbox]').element as HTMLInputElement).disabled).toBe(true)
    wrapper.unmount()
  })
})
