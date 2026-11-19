import { DEFAULT_THEME_COLOR, THEME_COLOR_KEY, THEME_MODE_KEY, isThemeColor, resolvePalette, variablesFor, type ThemeColor } from './tokens.mjs'

export type ColorMode = 'light' | 'dark' | 'system'
export interface AppearanceState {
  readonly color: ThemeColor
  readonly mode: ColorMode
  readonly isDark: boolean
  readonly storageAvailable: boolean
}

// No Vue, backend settings, identity, HTTP, or database dependencies. A page has
// one controller; Vue consumers subscribe and dispose their own subscriptions.
export function createAppearance(win: Window) {
  const root = win.document.documentElement
  const media = win.matchMedia?.('(prefers-color-scheme: dark)')
  const modernMediaEvents = typeof media?.addEventListener === 'function'
  const listeners = new Set<(value: AppearanceState) => void>()
  let state: AppearanceState = { color: DEFAULT_THEME_COLOR, mode: 'system', isDark: false, storageAvailable: true }
  let disposed = false
  let appliedColor: ThemeColor | undefined

  function apply(color: ThemeColor, mode: ColorMode, storageAvailable: boolean) {
    if (disposed) return
    const isDark = mode === 'dark' || (mode === 'system' && (media?.matches ?? false))
    if (appliedColor !== color) {
      for (const [name, value] of Object.entries(variablesFor(color))) root.style.setProperty(name, value)
      root.dataset.themeColor = color
      appliedColor = color
    }
    root.classList.toggle('dark', isDark)
    state = Object.freeze({ color, mode, isDark, storageAvailable })
    for (const listener of listeners) listener(state)
  }
  function sync() {
    try {
      const color = resolvePalette(win.localStorage.getItem(THEME_COLOR_KEY)).key
      const saved = win.localStorage.getItem(THEME_MODE_KEY)
      apply(color, saved === 'dark' || saved === 'light' ? saved : 'system', true)
    } catch {
      // A privacy policy/quota error must not prevent the app from rendering.
      apply(state.color, state.mode, false)
    }
  }
  function persist(key: string, value: string | null): boolean {
    try {
      if (value === null) win.localStorage.removeItem(key)
      else win.localStorage.setItem(key, value)
      return true
    } catch { return false }
  }
  function storageChanged(event: StorageEvent) {
    if (event.key !== null && event.key !== THEME_COLOR_KEY && event.key !== THEME_MODE_KEY) return
    try { if (event.storageArea && event.storageArea !== win.localStorage) return } catch { return }
    sync() // Read the real storage value; never persist while handling an event.
  }
  function systemChanged() {
    if (state.mode === 'system') apply(state.color, state.mode, state.storageAvailable)
  }
  win.addEventListener('storage', storageChanged)
  win.addEventListener('pageshow', sync)
  if (modernMediaEvents) media.addEventListener('change', systemChanged)
  else media?.addListener?.(systemChanged)
  sync()

  return {
    get state() { return state },
    setColor(key: unknown): boolean {
      if (disposed || !isThemeColor(key)) return false
      apply(key, state.mode, persist(THEME_COLOR_KEY, key))
      return true
    },
    setMode(mode: ColorMode) {
      if (disposed || !['light', 'dark', 'system'].includes(mode)) return
      apply(state.color, mode, persist(THEME_MODE_KEY, mode === 'system' ? null : mode))
    },
    toggleDark() { this.setMode(state.isDark ? 'light' : 'dark') },
    subscribe(listener: (value: AppearanceState) => void) {
      listeners.add(listener)
      listener(state)
      return () => { listeners.delete(listener) }
    },
    dispose() {
      disposed = true
      win.removeEventListener('storage', storageChanged)
      win.removeEventListener('pageshow', sync)
      if (modernMediaEvents) media.removeEventListener?.('change', systemChanged)
      else media?.removeListener?.(systemChanged)
      listeners.clear()
    },
  }
}

let instance: ReturnType<typeof createAppearance> | undefined
export function appearance() { return instance ??= createAppearance(window) }
export function disposeAppearance() { instance?.dispose(); instance = undefined }
if (import.meta.hot) import.meta.hot.dispose(disposeAppearance)
