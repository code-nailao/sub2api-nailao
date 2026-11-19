import { spyOnStorage } from './storageSpy'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createAppearance, disposeAppearance } from '../../../../../extensions/sources-brand/appearance'
import { palettes, variablesFor } from '../../../../../extensions/sources-brand/tokens.mjs'

function storageEvent(key: string | null, area: Storage = localStorage) {
  const event = new StorageEvent('storage', { key })
  // The repository's Node test setup uses an in-memory Storage implementation.
  Object.defineProperty(event, 'storageArea', { value: area })
  window.dispatchEvent(event)
}

let controller: ReturnType<typeof createAppearance>
let darkSystem = false
let systemChange: (() => void) | undefined

beforeEach(() => {
  disposeAppearance()
  localStorage.clear()
  document.documentElement.className = ''
  document.documentElement.removeAttribute('style')
  delete document.documentElement.dataset.themeColor
  darkSystem = false
  systemChange = undefined
  vi.stubGlobal('matchMedia', vi.fn(() => ({
    get matches() { return darkSystem },
    addEventListener: (_: string, fn: () => void) => { systemChange = fn },
    removeEventListener: () => { systemChange = undefined },
  })))
})
afterEach(() => { controller?.dispose(); disposeAppearance(); vi.restoreAllMocks(); vi.unstubAllGlobals() })

describe('shared browser-local appearance', () => {
  it('initializes the current default before mounting, without creating a stored preference', () => {
    controller = createAppearance(window)
    expect(controller.state.color).toBe('yellow')
    expect(document.documentElement.dataset.themeColor).toBe('yellow')
    expect(localStorage.getItem('theme-color')).toBeNull()
  })
  it.each(palettes)('keeps the legacy $key preference and all CSS variables', palette => {
    localStorage.setItem('theme-color', palette.key)
    controller = createAppearance(window)
    expect(controller.state.color).toBe(palette.key)
    for (const [name, value] of Object.entries(variablesFor(palette.key))) {
      expect(document.documentElement.style.getPropertyValue(name)).toBe(value)
    }
  })
  it('only accepts named palettes, not arbitrary CSS or unknown data', () => {
    localStorage.setItem('theme-color', 'not-a-palette')
    controller = createAppearance(window)
    expect(controller.state.color).toBe('yellow')
    expect(controller.setColor('url(https://example.invalid)')).toBe(false)
    expect(localStorage.getItem('theme-color')).toBe('not-a-palette')
  })
  it('persists and restores independently of the light/dark preference', () => {
    controller = createAppearance(window)
    controller.setColor('red'); controller.setMode('dark'); controller.dispose()
    controller = createAppearance(window)
    expect(controller.state).toMatchObject({ color: 'red', mode: 'dark', isDark: true })
    controller.setColor('slate')
    expect(localStorage.getItem('theme')).toBe('dark')
    controller.toggleDark()
    expect(localStorage.getItem('theme-color')).toBe('slate')
    expect(document.documentElement.classList.contains('dark')).toBe(false)
  })
  it('syncs other same-origin documents without writing back or reacting to auth events', () => {
    controller = createAppearance(window)
    localStorage.setItem('theme-color', 'teal')
    const writes = spyOnStorage('setItem')
    window.dispatchEvent(new StorageEvent('storage', { key: 'auth_token' }))
    expect(controller.state.color).toBe('yellow')
    storageEvent('theme-color')
    expect(controller.state.color).toBe('teal')
    expect(writes).not.toHaveBeenCalled()
  })
  it('ignores sessionStorage and resets on localStorage.clear', () => {
    controller = createAppearance(window); controller.setColor('red')
    localStorage.setItem('theme-color', 'slate')
    storageEvent('theme-color', sessionStorage)
    expect(controller.state.color).toBe('red')
    localStorage.clear(); storageEvent(null)
    expect(controller.state.color).toBe('yellow')
  })
  it('follows OS changes only until an explicit mode is chosen', () => {
    controller = createAppearance(window)
    darkSystem = true; systemChange?.()
    expect(controller.state.isDark).toBe(true)
    controller.setMode('light'); systemChange?.()
    expect(controller.state.isDark).toBe(false)
    controller.setMode('system')
    expect(controller.state.isDark).toBe(true)
    expect(localStorage.getItem('theme')).toBeNull()
  })
  it('supports older media listeners and read-only media-query implementations', () => {
    const addListener = vi.fn(), removeListener = vi.fn()
    vi.stubGlobal('matchMedia', vi.fn(() => ({ matches: true, addListener, removeListener })))
    controller = createAppearance(window)
    expect(controller.state.isDark).toBe(true)
    expect(addListener).toHaveBeenCalledOnce()
    controller.dispose()
    expect(removeListener).toHaveBeenCalledWith(addListener.mock.calls[0][0])
    vi.stubGlobal('matchMedia', vi.fn(() => ({ matches: false })))
    controller = createAppearance(window)
    expect(controller.state.isDark).toBe(false)
  })
  it('resyncs after returning from the page cache', () => {
    controller = createAppearance(window)
    localStorage.setItem('theme-color', 'orange')
    window.dispatchEvent(new Event('pageshow'))
    expect(controller.state.color).toBe('orange')
  })
  it('keeps working when reading browser storage is blocked', () => {
    spyOnStorage('getItem').mockImplementation(() => { throw new DOMException('blocked', 'SecurityError') })
    controller = createAppearance(window)
    expect(controller.state).toMatchObject({ color: 'yellow', storageAvailable: false })
    expect(document.documentElement.style.getPropertyValue('--color-primary-500')).toBe('#f2c14e')
  })
  it('applies an in-memory selection when writes fail, and reports that it is temporary', () => {
    controller = createAppearance(window)
    spyOnStorage('setItem').mockImplementation(() => { throw new DOMException('quota', 'QuotaExceededError') })
    expect(controller.setColor('sources-blue')).toBe(true)
    expect(controller.state).toMatchObject({ color: 'sources-blue', storageAvailable: false })
    expect(document.documentElement.style.getPropertyValue('--color-primary-600')).toBe('#0096c7')
  })
  it('notifies both consumers immediately and unsubscribes without leaking listeners', () => {
    controller = createAppearance(window)
    const one = vi.fn(), two = vi.fn()
    const unsubscribe = controller.subscribe(one); controller.subscribe(two)
    controller.setColor('forest-green'); expect(one).toHaveBeenCalledTimes(2); expect(two).toHaveBeenCalledTimes(2)
    unsubscribe(); controller.setColor('teal'); expect(one).toHaveBeenCalledTimes(2); expect(two).toHaveBeenCalledTimes(3)
    controller.dispose(); window.dispatchEvent(new Event('pageshow'))
    expect(two).toHaveBeenCalledTimes(3)
    expect(controller.setColor('slate')).toBe(false)
  })
})
