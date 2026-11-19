import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import { nextTick } from 'vue'
import PendingChargesNotice from '../PendingChargesNotice.vue'
import { listPendingCharges, type PendingCharge, type PendingChargesPage } from '@/api/pendingCharges'
import en from '@/i18n/locales/en/pendingCharges'
import zh from '@/i18n/locales/zh/pendingCharges'

vi.mock('@/api/pendingCharges', () => ({ listPendingCharges: vi.fn() }))
const list = vi.mocked(listPendingCharges)
const wrappers: VueWrapper[] = []

function row(overrides: Partial<PendingCharge> = {}): PendingCharge {
  return {
    request_id: 'req-reserved', model: 'grok-test', created_at: '2026-11-03T01:00:00Z',
    state: 'reserved', held_price: 0.1, ...overrides,
  }
}

function page(items: PendingCharge[], next_cursor: string | null = null): PendingChargesPage {
  return { items, next_cursor, page_size: 20 }
}

function render(locale = 'zh') {
  const wrapper = mount(PendingChargesNotice, {
    attachTo: document.body,
    global: { plugins: [createI18n({ legacy: false, locale, messages: { en, zh } })] },
  })
  wrappers.push(wrapper)
  return wrapper
}

function button(wrapper: VueWrapper, text: string) {
  const found = wrapper.findAll('button').find(item => item.text() === text)
  if (!found) throw new Error(`Button not found: ${text}`)
  return found
}

beforeEach(() => list.mockReset())
afterEach(() => {
  for (const wrapper of wrappers.splice(0)) wrapper.unmount()
})

describe('PendingChargesNotice', () => {
  it('hides a genuinely empty result without inventing usage rows or charges', async () => {
    let finish!: (value: PendingChargesPage) => void
    list.mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const wrapper = render()
    await nextTick()
    expect(wrapper.find('[role="status"]').text()).toContain('正在读取')
    expect(wrapper.find('h2').text()).toBe('待结算')
    finish(page([]))
    await flushPromises()
    expect(wrapper.find('section').exists()).toBe(false)
    expect(list).toHaveBeenCalledWith({ page_size: 20 }, { signal: expect.any(AbortSignal) })
  })

  it.each([{ status: 404 }, { response: { status: 404 } }])('hides the notice for an unsupported old backend: %j', async failure => {
    list.mockRejectedValueOnce(failure)
    const wrapper = render()
    await flushPromises()
    expect(wrapper.find('section').exists()).toBe(false)
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  it.each([401, 403, 500, undefined])('shows status %s failures and allows a retry without claiming no records', async status => {
    list.mockRejectedValueOnce({ status }).mockResolvedValueOnce(page([row()]))
    const wrapper = render()
    await flushPromises()
    expect(wrapper.find('[role="alert"]').text()).toBe('无法读取待结算记录，请重试。')
    expect(wrapper.text()).not.toContain('已加载全部')
    await button(wrapper, '重试').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('已加载 1 条记录，预留金额 ¥0.10')
    expect(list).toHaveBeenCalledTimes(2)
  })

  it('discloses unknown as not finally settled and expands a semantic request list', async () => {
    list.mockResolvedValueOnce(page([row({ request_id: 'req-unknown', state: 'unknown', held_price: 0.12345678 })]))
    const wrapper = render()
    await flushPromises()
    expect(wrapper.find('h2').text()).toBe('待结算')
    expect(wrapper.text()).toContain('已加载 1 条记录，预留金额 ¥0.12345678')
    const explanation = wrapper.findAll('p').find(item => item.text() === '相关金额已预留，最终费用以结算结果为准。')!
    expect(explanation.isVisible()).toBe(true)
    expect(wrapper.text()).toContain('结算结果待确认，相关请求尚未完成最终结算。')
    const toggle = button(wrapper, '查看待结算记录')
    expect(toggle.attributes('aria-expanded')).toBe('false')
    const regionId = toggle.attributes('aria-controls')
    const details = wrapper.findAll('div').find(item => item.attributes('id') === regionId)!
    expect(details.isVisible()).toBe(false)
    await toggle.trigger('click')
    expect(toggle.attributes('aria-expanded')).toBe('true')
    expect(details.isVisible()).toBe(true)
    expect(wrapper.findAll('[data-testid="pending-charge-row"]')).toHaveLength(1)
    expect(wrapper.find('dl').text()).toContain('req-unknown')
    expect(wrapper.find('dl').text()).toContain('grok-test')
    expect(wrapper.find('dl').text()).toContain('结算待确认')
    expect(wrapper.findAll('dd').some(item => item.text() === '¥0.12345678')).toBe(true)
    expect(wrapper.find('time').attributes('datetime')).toBe('2026-11-03T01:00:00Z')
    expect(wrapper.findAll('table')).toHaveLength(0)
    expect(wrapper.text()).not.toContain('退款')
    expect(wrapper.text()).not.toMatch(/待核对|上游|供应商|成本/)
    await toggle.trigger('click')
    expect(details.isVisible()).toBe(false)
  })

  it('pages with the exact opaque cursor and labels the amount as only loaded records', async () => {
    const cursor = 'opaque+/next==&cursor'
    list.mockResolvedValueOnce(page([row()], cursor))
      .mockResolvedValueOnce(page([row({ request_id: 'req-sent', state: 'sent', held_price: 0.2 })]))
    const wrapper = render('en')
    await flushPromises()
    expect(wrapper.find('h2').text()).toBe('Pending settlement')
    expect(wrapper.text()).toContain('Funds have been reserved. Final charges will be determined at settlement.')
    await button(wrapper, 'View pending settlement records').trigger('click')
    expect(wrapper.text()).toContain('Loaded records: 1 · Reserved amount: ¥0.10')
    expect(wrapper.text()).toContain('includes only loaded records')
    await button(wrapper, 'Load more').trigger('click')
    await flushPromises()
    expect(list).toHaveBeenLastCalledWith({ page_size: 20, cursor }, { signal: expect.any(AbortSignal) })
    expect(wrapper.findAll('[data-testid="pending-charge-row"]')).toHaveLength(2)
    expect(wrapper.text()).toContain('Loaded records: 2 · Reserved amount: ¥0.30')
    expect(wrapper.text()).toContain('Request submitted')
    expect(wrapper.text()).toContain('All pending settlement records loaded')
    expect(wrapper.findAll('button').some(item => item.text() === 'Load more')).toBe(false)
  })

  it('preserves loaded records after a page failure and retries the same cursor', async () => {
    list.mockResolvedValueOnce(page([row()], 'retry-cursor'))
      .mockRejectedValueOnce({ status: 503 })
      .mockResolvedValueOnce(page([row({ request_id: 'req-next', held_price: 0.25 })]))
    const wrapper = render()
    await flushPromises()
    await button(wrapper, '查看待结算记录').trigger('click')
    await button(wrapper, '加载更多').trigger('click')
    await flushPromises()
    expect(wrapper.find('[role="alert"]').text()).toContain('无法读取更多')
    expect(wrapper.text()).toContain('已加载 1 条记录，预留金额 ¥0.10')
    expect(wrapper.text()).not.toContain('已加载全部')
    await button(wrapper, '重试').trigger('click')
    await flushPromises()
    expect(list.mock.calls[1][0]).toEqual(list.mock.calls[2][0])
    expect(wrapper.text()).toContain('已加载 2 条记录，预留金额 ¥0.35')
    expect(wrapper.find('[role="alert"]').exists()).toBe(false)
  })

  it('refreshes from the first page and removes records that have been resolved', async () => {
    list.mockResolvedValueOnce(page([row()], 'old-cursor')).mockResolvedValueOnce(page([]))
    const wrapper = render()
    await flushPromises()
    await button(wrapper, '查看待结算记录').trigger('click')
    await button(wrapper, '刷新待结算记录').trigger('click')
    await flushPromises()
    expect(list).toHaveBeenLastCalledWith({ page_size: 20 }, { signal: expect.any(AbortSignal) })
    expect(wrapper.find('section').exists()).toBe(false)
  })

  it('deduplicates a request overlapping pages without double-counting its hold', async () => {
    list.mockResolvedValueOnce(page([row()], 'next'))
      .mockResolvedValueOnce(page([row({ state: 'unknown' }), row({ request_id: 'req-new', held_price: 0.2 })]))
    const wrapper = render()
    await flushPromises()
    await button(wrapper, '查看待结算记录').trigger('click')
    await button(wrapper, '加载更多').trigger('click')
    await flushPromises()
    expect(wrapper.findAll('[data-testid="pending-charge-row"]')).toHaveLength(2)
    expect(wrapper.text()).toContain('已加载 2 条记录，预留金额 ¥0.30')
    expect(wrapper.text()).toContain('结算结果待确认，相关请求尚未完成最终结算。')
  })

  it('prevents overlapping page requests and aborts an in-flight read when unmounted', async () => {
    let finish!: (value: PendingChargesPage) => void
    list.mockResolvedValueOnce(page([row()], 'next'))
      .mockImplementationOnce(() => new Promise(resolve => { finish = resolve }))
    const wrapper = render()
    await flushPromises()
    await button(wrapper, '查看待结算记录').trigger('click')
    await button(wrapper, '加载更多').trigger('click')
    expect(button(wrapper, '正在加载…').attributes('disabled')).toBeDefined()
    expect(button(wrapper, '刷新待结算记录').attributes('disabled')).toBeDefined()
    expect(wrapper.find('section').attributes('aria-busy')).toBe('true')
    const signal = list.mock.calls[1][1]!.signal!
    wrapper.unmount()
    expect(signal.aborted).toBe(true)
    finish(page([]))
    await flushPromises()
    expect(list).toHaveBeenCalledTimes(2)
  })
})
