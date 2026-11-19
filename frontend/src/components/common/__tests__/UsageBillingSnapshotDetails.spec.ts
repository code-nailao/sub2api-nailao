import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import UsageBillingSnapshotDetails from '../UsageBillingSnapshotDetails.vue'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, params?: { price?: string }) => params?.price ? `${key}: ${params.price}` : key }) }))

describe('historical billing evidence', () => {
  it('does not invent evidence for a legacy row', () => {
    expect(mount(UsageBillingSnapshotDetails).find('[data-testid="billing-snapshot"]').exists()).toBe(false)
  })
  it('retains normal tier evidence', () => {
    const wrapper = mount(UsageBillingSnapshotDetails, { props: { snapshot: {
      version: 1, requested_tier: 'default', forwarded_tier: 'ultrafast', observed_tier: 'priority',
      billed_tier: 'priority', tier_multiplier: 2, charged_tier_multiplier: 1,
      group_multiplier: 0.12, effective_rate_multiplier: 0.12, free_fast_applied: true,
      effective_unit_prices: { input: 0.00002 },
    } } })
    expect(wrapper.text()).toContain('ultrafast')
    expect(wrapper.text()).toContain('0.12×')
    expect(wrapper.text()).toContain('usage.freeFastApplied')
  })
  it('renders the public two-field snapshot without a schema version', () => {
    const wrapper = mount(UsageBillingSnapshotDetails, { props: { snapshot: {
      charge_kind: 'content_rejection', fixed_charge: 0.1,
    } } })
    expect(wrapper.get('[data-testid="rejection-charge"]').text()).toContain('0.10')
    expect(wrapper.text()).toContain('upstreamRejection.hint: 0.10')
    expect(wrapper.text()).not.toContain('×')
  })
  it.each(['content_rejection', 'upstream_rejection'])('never renders technical metadata supplied by a legacy/admin %s response', charge_kind => {
    const snapshot = {
      version: 1, charge_kind, fixed_charge: 0.1,
      original_status: 403, rejection_rule_id: 'private-internal-rule', rejection_rule_version: 'legacy_profile_v0',
      upstream_cost: 4.56789123, forwarded_tier: 'priority', observed_tier: 'priority', billed_tier: 'priority',
      tier_multiplier: 6, group_multiplier: 9,
    }
    const wrapper = mount(UsageBillingSnapshotDetails, { props: { snapshot } })
    expect(wrapper.get('[data-testid="rejection-charge"]').text()).toContain('0.10')
    for (const value of ['403', 'private-internal-rule', 'legacy_profile_v0', 'grok_content_rejection_v1', '4.56789123', 'priority', '9×', '6×']) {
      expect(wrapper.html()).not.toContain(value)
    }
  })
  it('does not invent an amount when a rejection snapshot has no price', () => {
    const wrapper = mount(UsageBillingSnapshotDetails, { props: { snapshot: { charge_kind: 'content_rejection' } } })
    expect(wrapper.get('[data-testid="rejection-charge"]').text()).toContain('usage.snapshotNotRecorded')
    expect(wrapper.text()).not.toContain('0.10')
  })
  it('keeps the Search and Fetch rows of a DeepSeek web tool receipt', () => {
    const wrapper = mount(UsageBillingSnapshotDetails, { props: { snapshot: { version: 1, charge_kind: 'deepseek_tools', deepseek_tools: {
      search_count: 2, fetch_count: 0, search_price_per_call: 0.006, fetch_price_per_call: 0.006,
    } } } })
    expect(wrapper.get('[data-testid="deepseek-tool-search"]').text()).toContain('0.012000u')
    expect(wrapper.get('[data-testid="deepseek-tool-fetch"]').text()).toContain('0.000000u')
    expect(wrapper.find('[data-testid="deepseek-tool-ocr"]').exists()).toBe(false)
  })
  it.each([
    ['ocr', { ocr_pages: 3, ocr_price_per_1k_pages: 3 }, 'deepseekTools.ocr × 3 deepseekTools.ocrUnit', '0.009000u'],
    ['asr', { asr_seconds: 62, asr_price_per_hour: 0.36 }, 'deepseekTools.asr × 62 deepseekTools.asrUnit', '0.006200u'],
    ['tts', { tts_characters: 8, tts_price_per_10k_chars: 2 }, 'deepseekTools.tts × 8 deepseekTools.ttsUnit', '0.001600u'],
  ])('shows the confirmed units of a standalone %s receipt', (tool, fields, label, amount) => {
    const wrapper = mount(UsageBillingSnapshotDetails, { props: { snapshot: { version: 1, charge_kind: 'deepseek_tools', deepseek_tools: {
      search_count: 0, fetch_count: 0, search_price_per_call: 0.006, fetch_price_per_call: 0.006, standalone: true, ...fields,
    } } } })
    const row = wrapper.get(`[data-testid="deepseek-tool-${tool}"]`).text()
    expect(row).toContain(label)
    expect(row).toContain(amount)
    expect(wrapper.find('[data-testid="deepseek-tool-search"]').exists()).toBe(false)
  })
  it('shows an uncharged standalone receipt as zero units', () => {
    const wrapper = mount(UsageBillingSnapshotDetails, { props: { snapshot: { version: 1, charge_kind: 'deepseek_tools', deepseek_tools: {
      search_count: 0, fetch_count: 0, search_price_per_call: 0.006, fetch_price_per_call: 0.006, standalone: true, tts_price_per_10k_chars: 2,
    } } } })
    expect(wrapper.get('[data-testid="deepseek-tool-tts"]').text()).toContain('0.000000u')
  })
})
