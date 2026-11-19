<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { palettes, resolvePalette, useAppearance } from './useAppearance'

const props = defineProps<{ collapsed: boolean }>()
const { t } = useI18n()
const { color, setColor, storageAvailable } = useAppearance()
const selected = computed(() => resolvePalette(color.value))
const open = ref(false)
const active = ref(0)
const container = ref<HTMLElement>()
const trigger = ref<HTMLButtonElement>()
const menu = ref<HTMLElement>()

function close(restoreFocus = false) {
  open.value = false
  if (restoreFocus) trigger.value?.focus()
}
async function toggle() {
  if (open.value) return close()
  active.value = palettes.findIndex(p => p.key === color.value)
  open.value = true
  await nextTick()
  focusOption()
}
function focusOption() { menu.value?.querySelectorAll<HTMLButtonElement>('[role="menuitemradio"]')[active.value]?.focus() }
function choose(index: number) { setColor(palettes[index].key); close(true) }
function keyboard(event: KeyboardEvent) {
  if (event.key === 'Escape') { event.preventDefault(); close(true); return }
  const next = event.key === 'Home' ? 0 : event.key === 'End' ? palettes.length - 1
    : ['ArrowDown', 'ArrowRight'].includes(event.key) ? (active.value + 1) % palettes.length
      : ['ArrowUp', 'ArrowLeft'].includes(event.key) ? (active.value + palettes.length - 1) % palettes.length : null
  if (next === null) return
  event.preventDefault()
  active.value = next
  focusOption()
}
function outside(event: PointerEvent) {
  if (!container.value?.contains(event.target as Node)) close()
}
function focusOut(event: FocusEvent) {
  if (!menu.value?.contains(event.relatedTarget as Node | null)) close()
}
watch(() => props.collapsed, () => close())
onMounted(() => document.addEventListener('pointerdown', outside))
onBeforeUnmount(() => document.removeEventListener('pointerdown', outside))
</script>

<template>
  <div ref="container" class="relative mb-2" @keydown.esc.prevent="close(true)">
    <button
      ref="trigger" type="button" class="sidebar-link w-full"
      :class="{ 'justify-center px-0': collapsed }"
      :aria-label="t('nav.themeColor')" :title="collapsed ? t('nav.themeColor') : undefined"
      aria-haspopup="menu" :aria-expanded="open" @click="toggle" @keydown.down.prevent="toggle"
    >
      <span class="theme-swatch shrink-0" :style="{ backgroundColor: selected.colors[500] }" aria-hidden="true"></span>
      <span v-if="!collapsed">{{ t('nav.themeColor') }}</span>
      <span v-if="!collapsed" class="ml-auto text-xs" aria-hidden="true">{{ open ? '▴' : '▾' }}</span>
    </button>
    <div
      v-if="open" ref="menu" role="menu" :aria-label="t('nav.themeColor')"
      class="theme-menu" :class="{ 'theme-menu-collapsed': collapsed }"
      @keydown="keyboard" @focusout="focusOut"
    >
      <button
        v-for="(palette, index) in palettes" :key="palette.key" type="button" role="menuitemradio"
        class="theme-option" :aria-checked="color === palette.key" :tabindex="active === index ? 0 : -1"
        :aria-label="palette.name" @click="choose(index)" @focus="active = index"
      >
        <span class="theme-swatch shrink-0" :style="{ backgroundColor: palette.colors[500] }" aria-hidden="true"></span>
        <span>{{ palette.name }}</span>
        <span v-if="color === palette.key" class="ml-auto" aria-hidden="true">✓</span>
      </button>
    </div>
    <span v-if="!storageAvailable" :class="collapsed ? 'sr-only' : 'mt-1 block px-2 text-xs text-amber-700 dark:text-amber-300'" role="status">{{ t('nav.themeColorTemporary') }}</span>
  </div>
</template>

<style scoped>
.theme-swatch { width: 1.125rem; height: 1.125rem; border-radius: 9999px; border: 2px solid white; box-shadow: 0 0 0 1px #94a3b8; }
.theme-menu {
  @apply absolute bottom-full left-0 right-0 z-50 mb-2 rounded-xl border border-gray-200 bg-white p-1.5 shadow-xl dark:border-dark-600 dark:bg-dark-900;
  /* Leave room for the fixed sidebar footer even on a landscape phone. */
  max-height: min(24rem, calc(100dvh - 13rem));
  overflow-y: auto;
  overscroll-behavior: contain;
}
.theme-menu-collapsed { left: calc(100% + 1rem); right: auto; bottom: 0; width: 12rem; }
.theme-option { @apply flex w-full items-center gap-2.5 rounded-lg px-3 py-2 text-left text-sm text-gray-700 dark:text-gray-200; }
.theme-option:hover, .theme-option:focus-visible { @apply bg-primary-50 text-primary-900 outline-none ring-2 ring-inset ring-primary-400 dark:bg-primary-900/40 dark:text-primary-100; }
.theme-option[aria-checked="true"] { @apply bg-primary-100 text-primary-900 dark:bg-primary-900/50 dark:text-primary-100; }
</style>
