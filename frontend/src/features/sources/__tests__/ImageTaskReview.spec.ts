import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import ImageTaskReview from '../ImageTaskReview.vue'
import zh from '@/i18n/locales/zh/common'

const api = vi.hoisted(() => ({ get: vi.fn(), post: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: api }))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<main><slot /></main>' } }))
vi.mock('@/components/auth/TotpStepUpDialog.vue', () => ({ default: { template: '<div />' } }))
vi.mock('@/composables/useStepUp', () => ({ useStepUp: () => ({ run: (fn: () => Promise<unknown>) => fn() }), isStepUpCancelled: () => false }))
const task = { id: `mediatask_${'a'.repeat(32)}`, phase: 'unknown', user_id: 1, api_key_id: 2, group_id: 74, account_id: 713, model: 'gpt-image-2', reserved_amount: 0.03, created_at: 1 }
let wrapper: ReturnType<typeof mount>
async function render(phase = 'unknown', recovery: Record<string, unknown> = {}) {
  api.get.mockResolvedValue({ data: { items: [{ ...task, phase, ...recovery }] } })
  wrapper = mount(ImageTaskReview, { attachTo: document.body, global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh: { sourcesImages: Object.fromEntries(Object.entries(zh.sourcesImages).map(([key, value]) => [key, () => value])) } } })] } })
  await flushPromises()
  return wrapper
}
afterEach(() => { wrapper?.unmount(); document.body.replaceChildren(); vi.resetAllMocks() })

describe('media engine administrator review', () => {
  it('shows automatic recovery only when reported by the backend, without a manual action', async () => {
    const w = await render('unknown', { recovery_status: 'recovering', recovery_deadline_at: 1800000000, manual_action_required: false })
    expect(w.get('h1').text()).toBe('图片异常处理')
    expect(w.text()).toContain('后台正在自动恢复，无需人工操作')
    expect(w.text()).toContain('自动处理期限')
    expect(w.findAll('button').some((button) => button.text() === '人工处理')).toBe(false)
    expect(api.post).not.toHaveBeenCalled()
  })
  it('keeps the manual fallback without pretending an older backend is recovering', async () => {
    const w = await render()
    expect(w.text()).not.toContain('后台正在自动恢复，无需人工操作')
    expect(w.text()).not.toContain('自动处理期限')
    expect(w.findAll('button').some((button) => button.text() === '人工处理')).toBe(true)
  })
  it('distinguishes a disabled policy from an active recovery', async () => {
    const w = await render('unknown', { recovery_status: 'disabled', manual_action_required: true })
    expect(w.text()).toContain('自动恢复尚未启用')
    expect(w.text()).not.toContain('后台正在自动恢复，无需人工操作')
    expect(w.findAll('button').some((button) => button.text() === '人工处理')).toBe(true)
  })
  it('does not offer another manual release while the original hold is closing', async () => {
    const w = await render('unknown', { recovery_status: 'closing', manual_action_required: false })
    expect(w.text()).toContain('后台正在解除原预留，无需重复操作')
    expect(w.findAll('button').some((button) => button.text() === '人工处理')).toBe(false)
    expect(api.post).not.toHaveBeenCalled()
  })
  it('does not mutate on listing, opening or cancellation', async () => {
    const w = await render()
    await w.findAll('button').find((b) => b.text() === '人工处理')!.trigger('click')
    await flushPromises()
    expect(document.querySelector('[type="submit"]')?.hasAttribute('disabled')).toBe(true)
    document.querySelector<HTMLButtonElement>('.modal-footer .btn-secondary')!.click()
    await flushPromises()
    expect(api.post).not.toHaveBeenCalled()
    expect(w.text()).toContain('0.03000000')
  })
  it('requires evidence and explicit confirmation; only original reference is submitted', async () => {
    const w = await render()
    await w.findAll('button').find((b) => b.text() === '人工处理')!.trigger('click')
    await flushPromises()
    const form = document.querySelector<HTMLFormElement>('#image-review-form')!
    const ref = form.querySelector<HTMLInputElement>('input:not([type="checkbox"])')!
    ref.value = 'verified_task'; ref.dispatchEvent(new Event('input', { bubbles: true }))
    const reason = form.querySelector('textarea')!
    reason.value = 'confirmed account and generation'; reason.dispatchEvent(new Event('input', { bubbles: true }))
    await flushPromises()
    expect(document.querySelector('[type="submit"]')?.hasAttribute('disabled')).toBe(true)
    form.querySelector<HTMLInputElement>('[type="checkbox"]')!.click()
    await flushPromises()
    api.post.mockResolvedValue({ data: { status: 'reconciliation_queued' } })
    form.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }))
    await flushPromises()
    expect(api.post).toHaveBeenCalledOnce()
    expect(api.post).toHaveBeenCalledWith(`/admin/sources/image-tasks/${task.id}/resolve`, { action: 'bind', reference: 'verified_task', reason: 'confirmed account and generation', confirm: true })
    expect(api.post.mock.calls[0][1]).not.toHaveProperty('amount')
  })
  it('only exposes delivery retry after a charged generation', async () => {
    const w = await render('delivery_review')
    await w.findAll('button').find((b) => b.text() === '人工处理')!.trigger('click')
    await flushPromises()
    const options = [...document.querySelectorAll('#image-review-form option')].map((el) => el.getAttribute('value'))
    expect(options).toEqual(['retry_delivery'])
  })
})
