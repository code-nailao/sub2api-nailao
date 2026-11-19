import { afterEach, describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import ChannelCard from '../ChannelCard.vue'
import { health, metric, row } from './fixtures'

vi.mock('vue-i18n', async (importOriginal) => ({ ...await importOriginal<typeof import('vue-i18n')>(), useI18n: () => ({ t: (key: string) => key, locale: { value: 'en' } }) }))
const wrappers: ReturnType<typeof mount>[] = []
afterEach(() => wrappers.splice(0).forEach(w => w.unmount()))
function timeline(count: number) {
  return Array.from({ length: count }, (_, i) => ({ bucket_start: new Date(Date.UTC(2026, 10, 17, 0, i * 5)).toISOString(), metrics: metric(), health: health() }))
}
describe('channel card keyboard timeline', () => {
  it('keeps one tab stop after switching from a longer to a shorter time window', async () => {
    const currentRow = { ...row(1), buckets: timeline(30) }
    const wrapper = mount(ChannelCard, { props: { row: currentRow, category: 'openai', coverage: null }, global: { stubs: { BaseDialog: true } } })
    wrappers.push(wrapper)
    await wrapper.findAll('.timeline-target')[29].trigger('focus')
    expect(wrapper.findAll('.timeline-target')[29].attributes('tabindex')).toBe('0')
    await wrapper.setProps({ row: { ...currentRow, buckets: timeline(18) } })
    expect(wrapper.findAll('.timeline-target[tabindex="0"]')).toHaveLength(1)
    expect(wrapper.findAll('.timeline-target')[17].attributes('tabindex')).toBe('0')
  })
})
