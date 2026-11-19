import { defineComponent, ref } from 'vue'
import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'
import DeepSeekToolAccountFields from '../DeepSeekToolAccountFields.vue'
import { deepSeekToolAccountForm } from '@/utils/deepseekTools'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string) => key }) }))

function fixture() {
  const form = ref(deepSeekToolAccountForm({ deepseek_tools_enabled: true, deepseek_search_cost_per_call: 0.005 }))
  const host = defineComponent({
    components: { DeepSeekToolAccountFields },
    setup: () => ({ form }),
    template: '<DeepSeekToolAccountFields v-model="form" base-url="https://provider.example/v1" />'
  })
  return { wrapper: mount(host), form }
}

describe('DeepSeek tool supplier costs', () => {
  it('are optional for all five tools and empty back to null', async () => {
    const { wrapper, form } = fixture()
    const inputs = wrapper.findAll('input[type=number]')
    expect(inputs).toHaveLength(5)
    expect(inputs.every(input => !(input.element as HTMLInputElement).required)).toBe(true)
    await inputs[0].setValue('')
    expect(form.value.deepseek_search_cost_per_call).toBe(null)
    await inputs[3].setValue('0.2')
    expect(form.value.deepseek_asr_cost_per_hour).toBe(0.2)
    expect(wrapper.text()).toContain('deepseekTools.costHint')
    expect(wrapper.text()).toContain('https://provider.example/v1/tools')
    // The native tool switch is gone: tools always run through the gateway.
    expect(wrapper.findAll('input[type=checkbox]')).toHaveLength(1)
    expect(wrapper.text()).not.toContain('nativeHint')
    wrapper.unmount()
  })
})
