import { shallowMount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import PricingEntryCard from '../PricingEntryCard.vue'
import type { PricingFormEntry } from '../types'

vi.mock('vue-i18n', async (importOriginal) => ({
  ...await importOriginal<typeof import('vue-i18n')>(),
  useI18n: () => ({ t: (key: string) => key }),
}))

describe('service tier presets', () => {
  const makeEntry = (models: string[]): PricingFormEntry => ({
    models, billing_mode: 'token', input_price: null, output_price: null,
    cache_read_price: null, cache_write_price: null, intervals: [],
    time_pricing: { timezone: 'Asia/Shanghai', weekdays_only: false, periods: [] },
  })
  it.each(['gpt-6-luna', 'openai/gpt-6-luna-high'])('offers only Fast for %s', async (model) => {
    const entry = makeEntry([model])
    const wrapper = shallowMount(PricingEntryCard, { props: { entry, enableServiceTiers: true } })
    expect(wrapper.find('[data-testid="ultrafast-field"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="flex-field"]').exists()).toBe(false)
    await wrapper.get('[data-testid="tier-preset-subscription"]').trigger('click')
    expect(wrapper.emitted('update')?.[0]?.[0]).toEqual({ ...entry, fast_multiplier: 2.5 })
  })
  it.each(['gpt-6-sol', 'gpt-5.6-sol', 'gpt-image-2', 'gpt-6-luna-preview'])('does not extend group tier controls to %s', (model) => {
    const wrapper = shallowMount(PricingEntryCard, { props: { entry: makeEntry([model]), enableServiceTiers: true } })
    expect(wrapper.find('[data-testid="tier-preset-api"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="ultrafast-field"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="flex-field"]').exists()).toBe(false)
  })
  it('preserves legacy channel Fast/Flex controls without adding Ultrafast', () => {
    const wrapper = shallowMount(PricingEntryCard, { props: { entry: makeEntry(['gpt-5.4']), enableTierMultipliers: true } })
    expect(wrapper.find('[data-testid="flex-field"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="ultrafast-field"]').exists()).toBe(false)
  })

  it.each([
    ['gpt-6.2-sol'], ['gpt-6.2-astra'],
    ['gpt-6.1-sol', 'gpt-6.2-sol'], ['gpt-6-astra', 'gpt-6.2-astra'],
    ['openai/GPT-6.2_SOL-max', 'gpt-6.2-astra-openai-compact'],
  ])('keeps both tier multipliers for %j without changing standard prices', async (...models) => {
    const entry = { ...makeEntry(models), input_price: 8, output_price: 40, cache_read_price: 0.4, cache_write_price: 10 }
    const wrapper = shallowMount(PricingEntryCard, { props: { entry, enableServiceTiers: true } })
    expect(wrapper.find('[data-testid="ultrafast-field"]').exists()).toBe(true)
    await wrapper.get('[data-testid="tier-preset-subscription"]').trigger('click')
    expect(wrapper.emitted('update')?.[0]?.[0]).toEqual({ ...entry, fast_multiplier: 2.5, ultrafast_multiplier: 8 })
    expect(entry).not.toHaveProperty('fast_multiplier')
    expect(entry).not.toHaveProperty('ultrafast_multiplier')
  })

  it.each(['gpt-6.2-solitude', 'gpt-6.2-astra-preview', 'gpt-6.2-luna', 'gpt-6.3-sol'])('does not guess tier support for %s', (model) => {
    const wrapper = shallowMount(PricingEntryCard, { props: { entry: makeEntry([model]), enableServiceTiers: true } })
    expect(wrapper.find('[data-testid="tier-preset-api"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="ultrafast-field"]').exists()).toBe(false)
  })

  it.each([
    ['api', 2, 6],
    ['subscription', 2.5, 8],
  ] as const)('changes only multipliers for %s', async (preset, fast, ultra) => {
    const entry: PricingFormEntry = {
      models: ['gpt-6-astra'], billing_mode: 'token', input_price: 10, output_price: 50,
      cache_read_price: 1, cache_write_price: 12.5, intervals: [],
      time_pricing: { timezone: 'Asia/Shanghai', weekdays_only: false, periods: [] },
    }
    const wrapper = shallowMount(PricingEntryCard, { props: { entry, enableTierMultipliers: true } })
    await wrapper.get(`[data-testid="tier-preset-${preset}"]`).trigger('click')
    expect(wrapper.emitted('update')).toHaveLength(1)
    expect(wrapper.emitted('update')?.[0]?.[0]).toEqual({
      ...entry, fast_multiplier: fast, ultrafast_multiplier: ultra,
    })
    expect(entry.input_price).toBe(10)
    expect(entry.output_price).toBe(50)
  })
})
