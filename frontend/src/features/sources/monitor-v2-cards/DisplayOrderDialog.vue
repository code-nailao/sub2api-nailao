<template>
  <BaseDialog :show="show" :title="t('channelMonitorV2.cards.sortTitle')" width="wide" :show-close-button="!saving" :close-on-escape="!saving" @close="emit('close')">
    <p class="mb-4 text-sm leading-relaxed text-gray-500 dark:text-gray-400">{{ t('channelMonitorV2.cards.sortHint') }}</p>
    <fieldset v-if="!loading && ready" class="mb-5 rounded-xl border border-gray-200 px-4 py-3 dark:border-dark-600" :disabled="saving">
      <legend class="px-1 text-sm font-medium text-gray-800 dark:text-gray-100">{{ t('channelMonitorV2.cards.imageGrouping') }}</legend>
      <div class="flex flex-wrap gap-x-6 gap-y-3 text-sm text-gray-700 dark:text-gray-200">
        <label class="inline-flex cursor-pointer items-center gap-2"><input v-model="imageGrouping" type="radio" name="monitor-image-grouping" value="separate" class="text-primary-600 focus:ring-primary-500" />{{ t('channelMonitorV2.cards.imageSeparate') }}</label>
        <label class="inline-flex cursor-pointer items-center gap-2"><input v-model="imageGrouping" type="radio" name="monitor-image-grouping" value="provider" class="text-primary-600 focus:ring-primary-500" />{{ t('channelMonitorV2.cards.imageProvider') }}</label>
      </div>
      <p class="mt-3 text-xs text-gray-500 dark:text-gray-400">{{ t('channelMonitorV2.cards.imageGroupingHint') }}</p>
    </fieldset>
    <p v-if="loading" class="py-12 text-center text-sm text-gray-500">{{ t('common.loading') }}</p>
    <div v-else-if="ready" class="grid min-w-0 gap-5 md:grid-cols-[170px_minmax(0,1fr)]">
      <nav class="flex gap-1 overflow-x-auto md:block md:space-y-1" :aria-label="t('channelMonitorV2.cards.sortScope')">
        <button type="button" class="sort-scope" :class="{ active: scope === 'categories' }" :disabled="saving" @click="scope = 'categories'"><Icon name="grid" size="sm" />{{ t('channelMonitorV2.cards.categoryOrder') }}</button>
        <button v-for="category in availableCategories" :key="category" type="button" class="sort-scope" :class="{ active: scope === category }" :disabled="saving" @click="scope = category"><Icon v-if="category === 'image'" name="image" size="sm" /><Icon v-else-if="category === 'other'" name="grid" size="sm" /><ProviderIcon v-else :provider="category === 'domestic' ? 'deepseek' : category" :size="17" />{{ categoryLabel(category) }}</button>
      </nav>
      <div class="min-w-0">
        <p class="mb-3 text-xs text-gray-500 dark:text-gray-400">{{ t('channelMonitorV2.cards.dragHint') }}</p>
        <VueDraggable v-if="scope === 'categories'" v-model="categoryItems" :animation="150" :disabled="saving" handle=".sort-grip" class="space-y-2">
          <div v-for="(category, index) in categoryItems" :key="category" :data-sort-category="category" class="sort-row">
            <span class="sort-grip cursor-grab touch-none text-gray-400"><Icon name="menu" size="sm" /></span><span class="sort-number">{{ index + 1 }}</span><Icon v-if="category === 'image'" name="image" size="sm" /><Icon v-else-if="category === 'other'" name="grid" size="sm" /><ProviderIcon v-else :provider="category === 'domestic' ? 'deepseek' : category" :size="21" /><strong class="min-w-0 flex-1 text-sm">{{ categoryLabel(category) }}</strong>
            <button type="button" class="btn btn-secondary !p-2" :disabled="saving || index === 0" :aria-label="t('channelMonitorV2.cards.moveUp', { name: categoryLabel(category) })" @click="moveCategory(index, -1)"><Icon name="arrowUp" size="sm" /></button><button type="button" class="btn btn-secondary !p-2" :disabled="saving || index === categoryItems.length - 1" :aria-label="t('channelMonitorV2.cards.moveDown', { name: categoryLabel(category) })" @click="moveCategory(index, 1)"><Icon name="arrowDown" size="sm" /></button>
          </div>
        </VueDraggable>
        <VueDraggable v-else v-model="currentGroups" :animation="150" :disabled="saving" handle=".sort-grip" class="space-y-2">
          <div v-for="(group, index) in currentGroups" :key="group.id" :data-sort-group="group.id" class="sort-row flex-wrap sm:flex-nowrap">
            <span class="sort-grip cursor-grab touch-none text-gray-400"><Icon name="menu" size="sm" /></span><span class="sort-number">{{ index + 1 }}</span><div class="min-w-0 flex-1"><strong class="block truncate text-sm" :title="group.name">{{ group.name }}</strong><small class="text-gray-500 dark:text-gray-400">{{ group.platform }} · #{{ group.id }}</small></div>
            <label class="group-category-label"><span class="sr-only">{{ t('channelMonitorV2.cards.groupCategory', { name: group.name }) }}</span><select class="input !w-28 !py-1.5 !text-xs" :value="draft.group_categories?.[String(group.id)] || 'auto'" :disabled="saving" @change="changeCategory(group.id, $event)"><option value="auto">{{ t('channelMonitorV2.cards.followDefault') }}</option><option v-for="category in DEFAULT_CATEGORIES" :key="category" :value="category">{{ categoryLabel(category) }}</option></select></label>
            <button type="button" class="btn btn-secondary !p-2" :disabled="saving || index === 0" :aria-label="t('channelMonitorV2.cards.moveUp', { name: group.name })" @click="moveGroup(index, -1)"><Icon name="arrowUp" size="sm" /></button><button type="button" class="btn btn-secondary !p-2" :disabled="saving || index === currentGroups.length - 1" :aria-label="t('channelMonitorV2.cards.moveDown', { name: group.name })" @click="moveGroup(index, 1)"><Icon name="arrowDown" size="sm" /></button>
          </div>
        </VueDraggable>
        <p v-if="scope !== 'categories' && !currentGroups.length" class="py-8 text-center text-sm text-gray-400">{{ t('channelMonitorV2.cards.emptyCategory') }}</p>
      </div>
    </div>
    <p v-if="error" class="mt-4 rounded-xl bg-red-50 px-4 py-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300" role="alert">{{ error }}</p>
    <template #footer><div class="flex flex-wrap items-center justify-between gap-3"><button type="button" class="btn btn-ghost btn-sm" :disabled="saving || loading || !ready" @click="draft = {}">{{ t('channelMonitorV2.cards.resetOrder') }}</button><div class="flex gap-3"><button type="button" class="btn btn-secondary" :disabled="saving" @click="emit('close')">{{ t('common.cancel') }}</button><button type="button" class="btn btn-primary" :disabled="saving || loading || !ready" @click="emit('save', cloneDisplayOrder(draft))">{{ saving ? t('common.saving') : t('common.save') }}</button></div></div></template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { VueDraggable } from 'vue-draggable-plus'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'
import ProviderIcon from '@/components/user/monitor/ProviderIcon.vue'
import type { MonitorCategory, MonitorDisplayOrder } from '@/api/channelMonitorV2'
import { DEFAULT_CATEGORIES, categoryOf, categoryOrder, cloneDisplayOrder, orderedGroups, replaceVisibleOrder, type DisplayCategory, type DisplayGroup } from './display'

const props = defineProps<{ show: boolean; loading: boolean; ready: boolean; saving: boolean; order?: MonitorDisplayOrder | null; groups: DisplayGroup[]; initialCategory?: DisplayCategory; error?: string }>()
const emit = defineEmits<{ close: []; save: [order: MonitorDisplayOrder] }>()
const { t } = useI18n()
const draft = ref<MonitorDisplayOrder>({})
const scope = ref<DisplayCategory | 'categories'>('categories')
watch(() => [props.show, props.order] as const, ([show]) => {
  if (show) { draft.value = cloneDisplayOrder(props.order); scope.value = props.initialCategory || 'categories' }
}, { immediate: true })
const categoryLabel = (category: DisplayCategory) => t(`channelMonitorV2.cards.categories.${category}`)
const imageGrouping = computed({
  get: () => draft.value.image_grouping || 'separate',
  set: (value: 'separate' | 'provider') => { if (!props.saving) draft.value.image_grouping = value },
})
const availableCategories = computed(() => categoryOrder(draft.value).filter(category => category !== 'other' || props.groups.some(group => categoryOf(group, draft.value) === 'other')))
const categoryItems = computed({
  get: () => availableCategories.value,
  set: value => {
    if (props.saving) return
    const visible = new Set(value), queue = [...value]
    draft.value.categories = categoryOrder(draft.value).map(category => visible.has(category) ? queue.shift()! : category)
  },
})
watch(availableCategories, categories => {
  if (scope.value !== 'categories' && !categories.includes(scope.value)) scope.value = 'categories'
})
const currentGroups = computed({
  get: () => scope.value === 'categories' ? [] : orderedGroups(props.groups, scope.value, draft.value),
  set: (groups: DisplayGroup[]) => {
    if (props.saving || scope.value === 'categories') return
    draft.value.groups = { ...draft.value.groups, [scope.value]: replaceVisibleOrder(draft.value.groups?.[scope.value] || [], groups.map(g => g.id)) }
  },
})
function moveCategory(index: number, delta: number) {
  if (props.saving) return
  const items = [...categoryItems.value], target = index + delta
  if (target < 0 || target >= items.length) return
  items.splice(target, 0, items.splice(index, 1)[0]); categoryItems.value = items
}
function moveGroup(index: number, delta: number) {
  if (props.saving) return
  const items = [...currentGroups.value], target = index + delta
  if (target < 0 || target >= items.length) return
  items.splice(target, 0, items.splice(index, 1)[0]); currentGroups.value = items
}
function changeCategory(id: number, event: Event) {
  if (props.saving) return
  const value = (event.target as HTMLSelectElement).value as MonitorCategory | 'auto'
  if (value !== 'auto' && !DEFAULT_CATEGORIES.includes(value)) return
  const overrides = { ...draft.value.group_categories }
  if (value === 'auto') delete overrides[String(id)]
  else overrides[String(id)] = value
  draft.value.group_categories = overrides
  const groups = { ...draft.value.groups }
  for (const category of DEFAULT_CATEGORIES) groups[category] = (groups[category] || []).filter(groupId => groupId !== id)
  if (value !== 'auto') groups[value] = [...(groups[value] || []), id]
  draft.value.groups = groups
}
</script>

<style scoped>
.sort-scope{display:flex;align-items:center;gap:9px;white-space:nowrap;width:100%;padding:11px 12px;border-radius:10px;font-size:13px;color:#64748b;text-align:left}.sort-scope:hover{background:#f1f5f9}.sort-scope.active{background:rgb(var(--color-primary-100-rgb, 219 234 254) / .6);color:rgb(var(--color-primary-700-rgb,29 78 216));font-weight:600}.sort-row{display:flex;align-items:center;gap:9px;padding:12px;border:1px solid #e5e7eb;border-radius:12px;color:#334155}.sort-number{font-size:11px;font-family:ui-monospace,monospace;color:#94a3b8;min-width:16px}.group-category-label{flex-shrink:0}.dark .sort-scope{color:#a9b5c6}.dark .sort-scope:hover{background:#1e293b}.dark .sort-scope.active{background:rgb(var(--color-primary-900-rgb,30 58 138) / .4);color:rgb(var(--color-primary-300-rgb,147 197 253))}.dark .sort-row{border-color:#334155;color:#e2e8f0}@media(max-width:639px){.group-category-label{margin-left:48px;width:calc(100% - 150px)}.sort-row{gap:8px}}
</style>
