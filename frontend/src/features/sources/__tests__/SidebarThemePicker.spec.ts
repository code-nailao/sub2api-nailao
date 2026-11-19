import { spyOnStorage } from './storageSpy'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { mount, flushPromises } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import SidebarThemePicker from '../SidebarThemePicker.vue'
import { disposeAppearance } from '../../../../../extensions/sources-brand/appearance'

let wrapper: ReturnType<typeof mount>
function render(collapsed = false) {
  wrapper = mount(SidebarThemePicker, {
    attachTo: document.body,
    props: { collapsed },
    global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh: { nav: { themeColor: () => '主题色', themeColorTemporary: () => '当前配色仅本次有效' } } } })] },
  })
  return wrapper
}
beforeEach(() => { disposeAppearance(); localStorage.clear(); delete document.documentElement.dataset.themeColor })
afterEach(() => { wrapper?.unmount(); disposeAppearance(); vi.restoreAllMocks(); document.body.innerHTML = '' })

describe('sidebar palette picker', () => {
  it('exposes the original eight options and marks the current selection', async () => {
    const w = render()
    await w.get('[aria-haspopup="menu"]').trigger('click'); await flushPromises()
    expect(w.findAll('[role="menuitemradio"]')).toHaveLength(8)
    expect(w.get('[aria-label="Yellow"]').attributes('aria-checked')).toBe('true')
    expect(document.activeElement?.getAttribute('aria-label')).toBe('Yellow')
    await w.get('[aria-label="Forest Green"]').trigger('click')
    expect(localStorage.getItem('theme-color')).toBe('forest-green')
    expect(w.find('[role="menu"]').exists()).toBe(false)
    expect(document.activeElement).toBe(w.get('[aria-haspopup="menu"]').element)
  })
  it('supports keyboard movement, Home/End and Escape without a focus trap', async () => {
    const w = render(); await w.get('[aria-haspopup="menu"]').trigger('click'); await flushPromises()
    await w.get('[role="menu"]').trigger('keydown', { key: 'ArrowDown' })
    expect(document.activeElement?.getAttribute('aria-label')).toBe('Red')
    await w.get('[role="menu"]').trigger('keydown', { key: 'Home' })
    expect(document.activeElement?.getAttribute('aria-label')).toBe('Blue')
    await w.get('[role="menu"]').trigger('keydown', { key: 'End' })
    expect(document.activeElement?.getAttribute('aria-label')).toBe('Slate')
    await w.get('[role="menu"]').trigger('keydown', { key: 'Escape' })
    expect(w.find('[role="menu"]').exists()).toBe(false)
    expect(document.activeElement).toBe(w.get('[aria-haspopup="menu"]').element)
  })
  it('works when collapsed and closes on outside clicks and layout changes', async () => {
    const w = render(true)
    expect(w.get('[aria-haspopup="menu"]').attributes('title')).toBe('主题色')
    await w.get('[aria-haspopup="menu"]').trigger('click'); await flushPromises()
    expect(w.get('[role="menu"]').classes()).toContain('theme-menu-collapsed')
    document.body.dispatchEvent(new Event('pointerdown', { bubbles: true })); await flushPromises()
    expect(w.find('[role="menu"]').exists()).toBe(false)
    await w.get('[aria-haspopup="menu"]').trigger('click'); await w.setProps({ collapsed: false })
    expect(w.find('[role="menu"]').exists()).toBe(false)
  })
  it('shows storage failures without losing the current page selection', async () => {
    const w = render()
    spyOnStorage('setItem').mockImplementation(() => { throw new Error('storage blocked') })
    await w.get('[aria-haspopup="menu"]').trigger('click'); await flushPromises()
    await w.get('[aria-label="Red"]').trigger('click')
    expect(document.documentElement.dataset.themeColor).toBe('red')
    expect(w.get('[role="status"]').text()).toBe('当前配色仅本次有效')
  })
})
