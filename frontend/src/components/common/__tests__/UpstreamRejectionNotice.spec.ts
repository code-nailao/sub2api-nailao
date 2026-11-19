import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { describe, expect, it } from 'vitest'
import UpstreamRejectionNotice from '../UpstreamRejectionNotice.vue'
import en from '@/i18n/locales/en/upstreamRejection'
import zh from '@/i18n/locales/zh/upstreamRejection'

const group = {
  platform: 'grok', subscription_type: 'standard',
  upstream_rejection_billing_enabled: true, upstream_rejection_price: 0.1,
  rate_multiplier: 2, user_rate_multiplier: 0.02, peak_rate_multiplier: 3,
}

function render(value = group, locale = 'zh') {
  return mount(UpstreamRejectionNotice, {
    props: { group: value },
    global: { plugins: [createI18n({ legacy: false, locale, messages: { en, zh } })] },
  })
}

describe('UpstreamRejectionNotice', () => {
  it('discloses the exact customer price and exclusive billing without applying multipliers', () => {
    const wrapper = render()
    expect(wrapper.text()).toContain('内容限制处理费')
    expect(wrapper.text()).toContain('¥0.10/次')
    expect(wrapper.text()).toContain('请求因内容限制被拒绝时，按 ¥0.10/次计费，不另收用量费，不受倍率影响。')
    for (const word of ['grok_content_rejection_v1', 'legacy_profile_v0', '上游', '供应商', '成本']) expect(wrapper.html()).not.toContain(word)
    expect(wrapper.text()).not.toContain('¥0.20')
  })

  it('updates from the public group price and supports English disclosure', async () => {
    const wrapper = render(group, 'en')
    expect(wrapper.text()).toContain('¥0.10 per request')
    expect(wrapper.text()).toContain('with no additional usage charge or multipliers')
    await wrapper.setProps({ group: { ...group, upstream_rejection_price: 0.12345678 } })
    expect(wrapper.text()).toContain('¥0.12345678 per request')
    await wrapper.setProps({ group: { ...group, upstream_rejection_billing_enabled: false } })
    expect(wrapper.find('[data-testid="upstream-rejection-notice"]').exists()).toBe(false)
  })

  it.each([0, -1, NaN, Infinity, 1e12, 0.123456789])('does not present invalid price %s as a billable offer', price => {
    expect(render({ ...group, upstream_rejection_price: price }).text()).toBe('')
  })

  it('does not invent a fee when older API responses omit the fields', () => {
    const wrapper = mount(UpstreamRejectionNotice, {
      props: { group: { platform: 'grok', subscription_type: 'standard' } },
      global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh } })] },
    })
    expect(wrapper.text()).toBe('')
  })

  it.each([
    { platform: 'openai', subscription_type: 'standard' },
    { platform: 'grok', subscription_type: 'subscription' },
  ])('does not display a fee outside the supported group scope: $platform/$subscription_type', scope => {
    expect(render({ ...group, ...scope }).text()).toBe('')
  })
})
