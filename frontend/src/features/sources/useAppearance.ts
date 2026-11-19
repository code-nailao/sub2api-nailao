import { computed, onScopeDispose, shallowRef } from 'vue'
import { appearance } from '../../../../extensions/sources-brand/appearance'

export { appearance } from '../../../../extensions/sources-brand/appearance'
export { palettes, resolvePalette } from '../../../../extensions/sources-brand/tokens.mjs'

export function useAppearance() {
  const controller = appearance()
  const state = shallowRef(controller.state)
  const unsubscribe = controller.subscribe(value => { state.value = value })
  onScopeDispose(unsubscribe)
  return {
    color: computed(() => state.value.color),
    isDark: computed(() => state.value.isDark),
    storageAvailable: computed(() => state.value.storageAvailable),
    setColor: (key: unknown) => controller.setColor(key),
    toggleTheme: () => controller.toggleDark(),
  }
}
