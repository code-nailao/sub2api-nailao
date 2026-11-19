import { afterEach, describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import MonitorOrderDialog from '../MonitorOrderDialog.vue'
import { makeMonitor } from './fixture'

const items = [makeMonitor({ id: 1, name: 'A' }), makeMonitor({ id: 2, name: 'B' })]
const wrappers: ReturnType<typeof mount>[] = []
function render() {
  const wrapper = mount(MonitorOrderDialog, {
    props: { show: true, items, submitting: false },
    global: {
      stubs: { teleport: true, VueDraggable: { props: ['modelValue'], template: '<div><slot /></div>' } },
      plugins: [createI18n({ legacy: false, locale: 'en', messages: { en: {
        common: { save: () => 'Save', saving: () => 'Saving', cancel: () => 'Cancel', enabled: () => 'Enabled' },
        admin: { channelMonitor: { sortOrder: () => 'Sort order', sortOrderHint: () => 'All monitors', moveUp: (c: { named: (key: string) => string }) => `Up ${c.named('name')}`, moveDown: (c: { named: (key: string) => string }) => `Down ${c.named('name')}` } },
      } } })],
    },
  })
  wrappers.push(wrapper)
  return wrapper
}
afterEach(() => { wrappers.splice(0).forEach(wrapper => wrapper.unmount()) })

describe('MonitorOrderDialog', () => {
  it('moves rows and saves only on explicit confirmation', async () => {
    const wrapper = render()
    await wrapper.get('button[aria-label="Down A"]').trigger('click')
    expect(wrapper.findAll('[data-monitor-id]').map(node => node.attributes('data-monitor-id'))).toEqual(['2', '1'])
    expect(items.map(item => item.id)).toEqual([1, 2])
    expect(wrapper.emitted('save')).toBeUndefined()
    await wrapper.findAll('button').find(button => button.text() === 'Save')!.trigger('click')
    expect(wrapper.emitted('save')?.[0]).toEqual([[items[1], items[0]]])
  })

  it('cancels without changing the saved input and resets on reopen', async () => {
    const wrapper = render()
    await wrapper.get('button[aria-label="Down A"]').trigger('click')
    await wrapper.findAll('button').find(button => button.text() === 'Cancel')!.trigger('click')
    expect(wrapper.emitted('save')).toBeUndefined()
    await wrapper.setProps({ show: false })
    await wrapper.setProps({ show: true })
    expect(wrapper.findAll('[data-monitor-id]').map(node => node.attributes('data-monitor-id'))).toEqual(['1', '2'])
  })

  it('blocks changes, duplicate saves and dismissal while saving', async () => {
    const wrapper = render()
    await wrapper.setProps({ submitting: true })
    expect(wrapper.find('button[aria-label="Close modal"]').exists()).toBe(false)
    expect(wrapper.findAll('button').every(button => button.attributes('disabled') !== undefined)).toBe(true)
    await wrapper.get('button[aria-label="Down A"]').trigger('click')
    expect(wrapper.findAll('[data-monitor-id]').map(node => node.attributes('data-monitor-id'))).toEqual(['1', '2'])
  })
})
