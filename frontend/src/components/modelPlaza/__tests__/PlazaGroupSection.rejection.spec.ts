import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { createI18n } from 'vue-i18n'
import { describe, expect, it } from 'vitest'
import PlazaGroupSection from '../PlazaGroupSection.vue'
import type { ModelPlazaGroup } from '@/api/modelPlaza'
import en from '@/i18n/locales/en/upstreamRejection'

const group: ModelPlazaGroup = {
  id: 247, name: 'Grok test', description: '', platform: 'grok', subscription_type: 'standard',
  rate_multiplier: 2, user_rate_multiplier: 0.01,
  peak_rate_enabled: false, peak_start: '', peak_end: '', peak_rate_multiplier: 1,
  is_exclusive: false, image_rate_independent: false, image_rate_multiplier: 1,
  video_rate_independent: false, video_rate_multiplier: 1,
  long_context_pricing_enabled: true,
  models: [{ name: 'grok-4.7', platform: 'grok', pricing: null, official_pricing: null }],
  upstream_rejection_billing_enabled: true, upstream_rejection_price: 0.1,
}

describe('PlazaGroupSection rejection price disclosure', () => {
  it('shows the same fixed group price next to models despite user and group multipliers', async () => {
    const wrapper = mount(PlazaGroupSection, {
      props: { group },
      global: {
        plugins: [createPinia(), createI18n({ legacy: false, locale: 'en', messages: { en } })],
        stubs: { Icon: true, GroupBadge: true, PlazaModelPricingTable: true },
      },
    })
    expect(wrapper.get('[data-testid="upstream-rejection-notice"]').text()).toContain('¥0.10 per request')
    expect(wrapper.get('plaza-model-pricing-table-stub').attributes('ratemultiplier')).toBe('2')
    await wrapper.setProps({ group: { ...group, upstream_rejection_billing_enabled: false } })
    expect(wrapper.find('[data-testid="upstream-rejection-notice"]').exists()).toBe(false)
  })
})
