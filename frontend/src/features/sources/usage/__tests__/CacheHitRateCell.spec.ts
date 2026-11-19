import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import CacheHitRateCell from '../CacheHitRateCell.vue'
import Icon from '@/components/icons/Icon.vue'

type Usage = InstanceType<typeof CacheHitRateCell>['$props']['usage']

const usage: Usage = {
  input_tokens: 200,
  cache_creation_tokens: 0,
  cache_read_tokens: 800,
}

function mountCell(value: Usage = usage, locale = 'zh') {
  return mount(CacheHitRateCell, {
    props: { usage: value },
    global: {
      plugins: [createI18n({
        legacy: false,
        locale,
        messages: {
          zh: { usage: { performanceCache: () => '缓存' } },
          en: { usage: { performanceCache: () => 'Cache' } },
        },
      })],
    },
  })
}

describe('CacheHitRateCell', () => {
  it('renders only cache hit rate, leaving output TPS to the native latency column', () => {
    const wrapper = mountCell()
    expect(wrapper.element.children).toHaveLength(1)
    expect(wrapper.get('[data-testid="cache-hit-rate"]').text()).toBe('缓存: 80.0%')
    expect(wrapper.text()).not.toContain('TPS')
    expect(wrapper.findAllComponents(Icon).map(icon => icon.props('name'))).toEqual(['database'])
    expect(wrapper.get('[data-testid="cache-hit-rate"]').classes()).toEqual(expect.arrayContaining([
      'text-blue-600', 'dark:text-blue-400',
    ]))
  })

  it('includes cache creation in the same prompt-token denominator as the trend chart', () => {
    const wrapper = mountCell({ ...usage, input_tokens: 200, cache_creation_tokens: 300, cache_read_tokens: 500 })
    expect(wrapper.get('[data-testid="cache-hit-rate"]').text()).toBe('缓存: 50.0%')
  })

  it.each([
    { input_tokens: 100, cache_creation_tokens: 0, cache_read_tokens: 0, expected: '0.0%' },
    { input_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 100, expected: '100.0%' },
    { input_tokens: 2, cache_creation_tokens: 0, cache_read_tokens: 1, expected: '33.3%' },
  ])('formats valid cache data as $expected', ({ expected, ...tokens }) => {
    const wrapper = mountCell({ ...usage, ...tokens })
    const cache = wrapper.get('[data-testid="cache-hit-rate"]')
    expect(cache.text()).toBe(`缓存: ${expected}`)
    expect(cache.classes()).toContain('text-blue-600')
  })

  it.each<Usage>([
    { input_tokens: undefined }, { input_tokens: null }, { input_tokens: -1 }, { input_tokens: NaN },
    { cache_creation_tokens: undefined }, { cache_creation_tokens: null }, { cache_creation_tokens: -1 },
    { cache_creation_tokens: Infinity }, { cache_read_tokens: undefined }, { cache_read_tokens: null },
    { cache_read_tokens: -1 }, { cache_read_tokens: Infinity },
    { input_tokens: 0, cache_creation_tokens: 0, cache_read_tokens: 0 },
    { input_tokens: Number.MAX_VALUE, cache_read_tokens: Number.MAX_VALUE },
  ])('shows unavailable cache data without inventing a zero hit rate: %j', invalid => {
    const wrapper = mountCell({ ...usage, ...invalid })
    expect(wrapper.get('[data-testid="cache-hit-rate"]').text()).toBe('缓存: -')
    expect(wrapper.get('[data-testid="cache-hit-rate"]').classes()).toContain('text-gray-400')
  })

  it('updates when a row is refreshed', async () => {
    const wrapper = mountCell()
    await wrapper.setProps({ usage: { ...usage, cache_read_tokens: 0 } })
    expect(wrapper.get('[data-testid="cache-hit-rate"]').text()).toBe('缓存: 0.0%')
    await wrapper.setProps({ usage: {} })
    expect(wrapper.get('[data-testid="cache-hit-rate"]').text()).toBe('缓存: -')
  })

  it('supports the existing English locale', () => {
    expect(mountCell(usage, 'en').get('[data-testid="cache-hit-rate"]').text()).toBe('Cache: 80.0%')
  })
})
