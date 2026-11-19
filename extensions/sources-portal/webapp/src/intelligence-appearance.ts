import { shallowRef } from 'vue'
import { appearance, type AppearanceState, type ColorMode } from '../../../sources-brand/appearance'
import { isThemeColor, variablesFor, type ThemeColor } from '../../../sources-brand/tokens.mjs'

const params = new URLSearchParams(location.search)
const validMode = (v: unknown): v is ColorMode => v === 'light' || v === 'dark' || v === 'system'
const suppliedMode = params.get('theme')
let modeOverride: ColorMode | undefined = validMode(suppliedMode) ? suppliedMode : undefined
const suppliedColor = params.get('theme_color')
let colorOverride: ThemeColor | undefined = isThemeColor(suppliedColor) ? suppliedColor : undefined
for (const key of ['token', 'user_id', 'src_url', 'src_host']) params.delete(key)
history.replaceState(null, '', location.pathname + (params.size ? '?' + params : '') + location.hash)

export const embedded = params.get('ui_mode') === 'embedded' && window.parent !== window
const shared = appearance()
const state = shallowRef(shared.state)
let lastBase = shared.state
const media = matchMedia('(prefers-color-scheme: dark)')

function apply(base: AppearanceState) {
  lastBase = base
  const color = colorOverride || base.color, mode = modeOverride || base.mode
  const isDark = mode === 'dark' || mode === 'system' && media.matches
  const root = document.documentElement
  for (const [name, value] of Object.entries(variablesFor(color))) root.style.setProperty(name, value)
  root.dataset.themeColor = color
  root.classList.toggle('dark', isDark)
  root.style.colorScheme = isDark ? 'dark' : 'light'
  state.value = { ...base, color, mode, isDark }
}

function clearOverride(key: string) {
  params.delete(key)
  history.replaceState(null, '', location.pathname + (params.size ? '?' + params : '') + location.hash)
}

shared.subscribe(apply)
media.addEventListener('change', () => apply(lastBase))
window.addEventListener('message', event => {
  if (!embedded || event.source !== window.parent || event.origin !== location.origin ||
      event.data?.type !== 'sources:intelligence-appearance' ||
      !validMode(event.data.mode) || !isThemeColor(event.data.color)) return
  apply({ ...shared.state, color: event.data.color, mode: event.data.mode })
})
if (embedded) window.parent.postMessage({ type: 'sources:intelligence-appearance-ready' }, location.origin)

export function intelligenceAppearance() {
  return {
    state,
    setMode(mode: ColorMode) {
      modeOverride = undefined
      clearOverride('theme')
      shared.setMode(mode)
    },
    setColor(color: ThemeColor) {
      colorOverride = undefined
      clearOverride('theme_color')
      shared.setColor(color)
    },
  }
}
