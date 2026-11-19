import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import UpstreamRejectionBillingFields from '../UpstreamRejectionBillingFields.vue'
vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))
describe('fixed rejection fee fields', () => {
  it('has explicit opt-in and editable price without multiplier input', async () => {
    const wrapper = mount(UpstreamRejectionBillingFields, { props: { enabled: false, price: 0.1, idPrefix: 'test' } })
    expect(wrapper.find('input[type="number"]').exists()).toBe(false)
    await wrapper.get('input[type="checkbox"]').setValue(true)
    expect(wrapper.emitted('update:enabled')?.[0]).toEqual([true])
    await wrapper.setProps({ enabled: true })
    await wrapper.get('input[type="number"]').setValue('0.2')
    expect(wrapper.emitted('update:price')?.[0]).toEqual([0.2])
    expect(wrapper.get('label[for="test-rejection-price"]').text()).toContain('price')
  })
  it('can disable an invalid fee without leaving an uneditable invalid hidden value', async () => {
    const wrapper = mount(UpstreamRejectionBillingFields, { props: { enabled: true, price: '', idPrefix: 'test' } })
    await wrapper.get('input[type="checkbox"]').setValue(false)
    expect(wrapper.emitted('update:enabled')?.[0]).toEqual([false])
    expect(wrapper.emitted('update:price')?.[0]).toEqual([0.1])
  })
  it('prevents unsupported activation but permits disabling a previously enabled policy', async () => {
    const wrapper = mount(UpstreamRejectionBillingFields, { props: { enabled: false, price: 0.1, idPrefix: 'test', eligible: false } })
    expect(wrapper.get('input[type="checkbox"]').attributes('disabled')).toBeDefined()
    await wrapper.setProps({ enabled: true })
    expect(wrapper.get('input[type="checkbox"]').attributes('disabled')).toBeUndefined()
    await wrapper.get('input[type="checkbox"]').setValue(false)
    expect(wrapper.emitted('update:enabled')?.[0]).toEqual([false])
  })
  it.each([0, -1, '', Number.NaN, Number.POSITIVE_INFINITY, 1e12, 0.100000001])('announces invalid enabled price %s', (price) => {
    const wrapper = mount(UpstreamRejectionBillingFields, { props: { enabled: true, price, idPrefix: 'test' } })
    expect(wrapper.get('[role="alert"]').text()).toContain('invalidPrice')
    expect(wrapper.get('input[type="number"]').attributes('aria-invalid')).toBe('true')
  })
})
