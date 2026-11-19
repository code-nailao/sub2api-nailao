import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import DisplayOrderDialog from '../DisplayOrderDialog.vue'
import type { MonitorDisplayOrder } from '@/api/channelMonitorV2'

vi.mock('vue-i18n', () => ({ useI18n: () => ({ t: (key: string, values?: Record<string, string>) => `${key}${values?.name ? `:${values.name}` : ''}` }) }))
const wrappers: ReturnType<typeof mount>[] = []
const groups = [{ id: 1, name: 'A', platform: 'openai' }, { id: 2, name: 'B', platform: 'openai' }, { id: 3, name: 'Domestic', platform: 'deepseek' }, { id: 4, name: 'GPT Image 2', platform: 'openai' }, { id: 5, name: 'Future', platform: 'future' }, { id: 6, name: 'Future 2', platform: 'future-2' }]
function render(initialCategory?: 'openai' | 'other' | 'image') {
  const order: MonitorDisplayOrder = { categories: ['openai', 'anthropic', 'grok', 'gemini', 'domestic'], groups: { openai: [1, 99, 2] } }
  const wrapper = mount(DisplayOrderDialog, {
    props: { show: true, loading: false, ready: true, saving: false, groups, order, initialCategory },
    global: { stubs: {
      BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' },
      VueDraggable: { props: ['modelValue'], template: '<div><slot /></div>' },
    } },
  })
  wrappers.push(wrapper)
  return { wrapper, order }
}
afterEach(() => wrappers.splice(0).forEach(w => w.unmount()))

describe('V2 display order editor', () => {
  it('saves category order only after confirmation and retains channels outside the catalog', async () => {
    const { wrapper, order } = render()
    await wrapper.get('button[aria-label="channelMonitorV2.cards.moveDown:channelMonitorV2.cards.categories.openai"]').trigger('click')
    expect(wrapper.findAll('[data-sort-category]').map(e => e.attributes('data-sort-category')).slice(0, 2)).toEqual(['anthropic', 'openai'])
    expect(wrapper.emitted('save')).toBeUndefined()
    expect(order.categories?.[0]).toBe('openai')
    await wrapper.findAll('button').find(b => b.text() === 'common.save')!.trigger('click')
    const saved = wrapper.emitted('save')?.[0]?.[0] as MonitorDisplayOrder
    expect(saved.categories?.[0]).toBe('anthropic')
    expect(saved.groups?.openai).toEqual([1, 99, 2])
  })

  it('reorders group IDs, cancels without mutation and resets when reopened', async () => {
    const { wrapper, order } = render('openai')
    await wrapper.get('button[aria-label="channelMonitorV2.cards.moveDown:A"]').trigger('click')
    expect(wrapper.findAll('[data-sort-group]').map(e => e.attributes('data-sort-group'))).toEqual(['2', '1'])
    expect(order.groups?.openai).toEqual([1, 99, 2])
    await wrapper.findAll('button').find(b => b.text() === 'common.cancel')!.trigger('click')
    expect(wrapper.emitted('save')).toBeUndefined()
    await wrapper.setProps({ show: false }); await wrapper.setProps({ show: true })
    expect(wrapper.findAll('[data-sort-group]').map(e => e.attributes('data-sort-group'))).toEqual(['1', '2'])
  })

  it('can explicitly move an OpenAI-protocol group into the domestic category', async () => {
    const { wrapper } = render('openai')
    await wrapper.get('[data-sort-group="1"] select').setValue('domestic')
    expect(wrapper.find('[data-sort-group="1"]').exists()).toBe(false)
    await wrapper.findAll('button').find(b => b.text() === 'common.save')!.trigger('click')
    const saved = wrapper.emitted('save')?.[0]?.[0] as MonitorDisplayOrder
    expect(saved.group_categories).toEqual({ '1': 'domestic' })
    expect(saved.groups?.openai).toEqual([99, 2])
    expect(saved.groups?.domestic).toEqual([1])
  })

  it('prevents duplicate changes and saves while a request is in flight', async () => {
    const { wrapper } = render('openai')
    await wrapper.setProps({ saving: true })
    expect(wrapper.findAll('button').every(b => b.attributes('disabled') !== undefined)).toBe(true)
    expect(wrapper.findAll('select').every(b => b.attributes('disabled') !== undefined)).toBe(true)
  })

  it('allows the other category and its channels to be moved and saved', async () => {
    const { wrapper } = render()
    await wrapper.get('button[aria-label="channelMonitorV2.cards.moveUp:channelMonitorV2.cards.categories.other"]').trigger('click')
    expect(wrapper.findAll('[data-sort-category]').map(e => e.attributes('data-sort-category')).slice(-2)).toEqual(['other', 'image'])
    await wrapper.findAll('.sort-scope').find(b => b.text() === 'channelMonitorV2.cards.categories.other')!.trigger('click')
    await wrapper.get('button[aria-label="channelMonitorV2.cards.moveDown:Future"]').trigger('click')
    expect(wrapper.findAll('[data-sort-group]').map(e => e.attributes('data-sort-group'))).toEqual(['6', '5'])
    await wrapper.findAll('button').find(b => b.text() === 'common.save')!.trigger('click')
    const saved = wrapper.emitted('save')?.[0]?.[0] as MonitorDisplayOrder
    expect(saved.categories?.slice(-2)).toEqual(['other', 'image'])
    expect(saved.groups?.other).toEqual([6, 5])
  })

  it('allows an administrator to explicitly place a regular group in other', async () => {
    const { wrapper } = render('openai')
    await wrapper.get('[data-sort-group="1"] select').setValue('other')
    await wrapper.findAll('button').find(b => b.text() === 'common.save')!.trigger('click')
    const saved = wrapper.emitted('save')?.[0]?.[0] as MonitorDisplayOrder
    expect(saved.group_categories?.['1']).toBe('other')
    expect(saved.groups?.other).toEqual([1])
  })

  it('hides empty other from category rows and navigation without losing its stored position', async () => {
    const { wrapper } = render()
    await wrapper.setProps({ groups: groups.filter(group => !group.platform.startsWith('future')), order: { categories: ['other', 'openai', 'anthropic', 'grok', 'gemini', 'domestic'] } })
    expect(wrapper.find('[data-sort-category="other"]').exists()).toBe(false)
    expect(wrapper.findAll('.sort-scope').some(button => button.text() === 'channelMonitorV2.cards.categories.other')).toBe(false)
    await wrapper.get('button[aria-label="channelMonitorV2.cards.moveDown:channelMonitorV2.cards.categories.openai"]').trigger('click')
    await wrapper.findAll('button').find(b => b.text() === 'common.save')!.trigger('click')
    const saved = wrapper.emitted('save')?.[0]?.[0] as MonitorDisplayOrder
    expect(saved.categories?.slice(0, 3)).toEqual(['other', 'anthropic', 'openai'])
  })

  it('switches image grouping both ways and saves the mode without inventing manual overrides', async () => {
    const { wrapper } = render('image')
    expect((wrapper.get('input[value="separate"]').element as HTMLInputElement).checked).toBe(true)
    expect(wrapper.find('[data-sort-group="4"]').exists()).toBe(true)
    await wrapper.get('input[value="provider"]').setValue()
    expect(wrapper.find('[data-sort-group="4"]').exists()).toBe(false)
    await wrapper.findAll('.sort-scope').find(button => button.text() === 'channelMonitorV2.cards.categories.openai')!.trigger('click')
    expect(wrapper.findAll('[data-sort-group]').map(element => element.attributes('data-sort-group'))).toEqual(['1', '2', '4'])
    await wrapper.get('input[value="separate"]').setValue()
    expect(wrapper.find('[data-sort-group="4"]').exists()).toBe(false)
    await wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')
    const saved = wrapper.emitted('save')?.[0]?.[0] as MonitorDisplayOrder
    expect(saved.image_grouping).toBe('separate')
    expect(saved.group_categories).toBeUndefined()
  })

  it('keeps manual categories through mode changes and restores automatic placement with follow default', async () => {
    const { wrapper } = render('image')
    await wrapper.get('[data-sort-group="4"] select').setValue('openai')
    await wrapper.get('input[value="provider"]').setValue()
    await wrapper.get('input[value="separate"]').setValue()
    await wrapper.findAll('.sort-scope').find(button => button.text() === 'channelMonitorV2.cards.categories.openai')!.trigger('click')
    expect(wrapper.find('[data-sort-group="4"]').exists()).toBe(true)
    await wrapper.get('[data-sort-group="4"] select').setValue('auto')
    expect(wrapper.find('[data-sort-group="4"]').exists()).toBe(false)
    await wrapper.findAll('.sort-scope').find(button => button.text() === 'channelMonitorV2.cards.categories.image')!.trigger('click')
    expect(wrapper.find('[data-sort-group="4"]').exists()).toBe(true)
    await wrapper.findAll('button').find(button => button.text() === 'common.save')!.trigger('click')
    const saved = wrapper.emitted('save')?.[0]?.[0] as MonitorDisplayOrder
    expect(saved.group_categories?.['4']).toBeUndefined()
    expect(Object.values(saved.groups || {}).every(ids => !ids?.includes(4))).toBe(true)
  })

  it('disables saving and hides editable settings when the configuration could not be read', async () => {
    const { wrapper } = render()
    await wrapper.setProps({ ready: false, error: 'Could not load configuration' })
    expect(wrapper.findAll('button').find(button => button.text() === 'common.save')!.attributes('disabled')).toBeDefined()
    expect(wrapper.find('input[type="radio"]').exists()).toBe(false)
    expect(wrapper.find('[data-sort-category]').exists()).toBe(false)
    expect(wrapper.get('[role="alert"]').text()).toContain('Could not load configuration')
  })
})
