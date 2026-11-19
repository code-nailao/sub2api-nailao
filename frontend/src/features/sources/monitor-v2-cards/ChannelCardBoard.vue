<template>
  <section class="space-y-5" :aria-label="t('channelMonitorV2.cards.title')">
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div class="flex flex-wrap items-center gap-2" role="group" :aria-label="t('channelMonitorV2.cards.categoryFilter')">
        <button type="button" class="category-tab" :class="{ selected: selectedCategory === 'all' }" :aria-pressed="selectedCategory === 'all'" @click="selectedCategory = 'all'"><Icon name="grid" size="sm" />{{ t('channelMonitorV2.cards.all') }}<span>{{ rows.length }}</span></button>
        <button v-for="category in categories" :key="category" type="button" class="category-tab" :class="{ selected: selectedCategory === category }" :aria-pressed="selectedCategory === category" @click="selectedCategory = category"><Icon v-if="category === 'image'" name="image" size="sm" /><Icon v-else-if="category === 'other'" name="grid" size="sm" /><ProviderIcon v-else :provider="category === 'domestic' ? 'deepseek' : category" :size="16" />{{ categoryLabel(category) }}<span>{{ rowsFor(category).length }}</span></button>
      </div>
      <div class="flex min-w-0 flex-wrap items-center gap-2">
        <label class="relative"><span class="sr-only">{{ t('channelMonitorV2.cards.search') }}</span><Icon name="search" size="sm" class="pointer-events-none absolute left-3 top-1/2 -translate-y-1/2 text-gray-400" /><input v-model="search" class="input w-44 !py-2 !pl-9 !text-xs sm:w-48" type="search" :placeholder="t('channelMonitorV2.cards.search')" /></label>
        <label v-if="isAdmin" class="inline-flex min-h-9 cursor-pointer items-center gap-2 px-1 text-xs text-gray-500 dark:text-gray-400"><input v-model="attentionOnly" type="checkbox" class="h-4 w-4 rounded border-gray-300 text-primary-600 focus:ring-primary-500" />{{ t('channelMonitorV2.cards.attentionOnly') }}</label>
        <button v-if="isAdmin" type="button" class="btn btn-secondary btn-sm" @click="openOrder()"><Icon name="menu" size="sm" />{{ t('channelMonitorV2.cards.sortTitle') }}</button>
      </div>
    </div>

    <p v-if="rateLoadFailed" class="text-xs text-amber-700 dark:text-amber-300" role="status">{{ t('channelMonitorV2.cards.ratesUnavailable') }}</p>

    <div v-if="loading && !rows.length" class="grid grid-cols-1 gap-5 md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4" aria-hidden="true"><div v-for="i in 8" :key="i" class="h-64 animate-pulse rounded-3xl bg-gray-100 dark:bg-dark-800"></div></div>
    <section v-for="section in sections" v-else :key="section.category" class="space-y-4" :aria-labelledby="`sources-monitor-${section.category}`">
      <header class="flex items-center justify-between gap-3 pt-2"><div class="flex min-w-0 items-center gap-2.5"><span v-if="section.category === 'domestic'" class="domestic-icons" aria-hidden="true"><span v-for="provider in ['deepseek', 'kimi', 'zhipu']" :key="provider"><ProviderIcon :provider="provider" :size="18" /></span></span><span v-else class="section-provider"><Icon v-if="section.category === 'image'" name="image" size="md" /><Icon v-else-if="section.category === 'other'" name="grid" size="md" /><ProviderIcon v-else :provider="section.category" :size="22" /></span><h2 :id="`sources-monitor-${section.category}`" class="text-lg font-bold text-gray-900 dark:text-white">{{ categoryLabel(section.category) }}</h2><span class="rounded-full bg-gray-100 px-2 py-0.5 text-xs tabular-nums text-gray-500 dark:bg-dark-700 dark:text-gray-300">{{ section.rows.length }}</span><span v-if="section.category === 'domestic'" class="hidden text-xs text-gray-400 sm:inline">DeepSeek · Kimi · GLM</span></div><button v-if="isAdmin" type="button" class="btn btn-ghost btn-sm !px-2 text-xs" :aria-label="t('channelMonitorV2.cards.sortGroup', { name: categoryLabel(section.category) })" @click="openOrder(section.category)"><Icon name="menu" size="xs" /><span>{{ t('channelMonitorV2.cards.adjustOrder') }}</span></button></header>
      <div class="grid grid-cols-1 gap-5 md:grid-cols-2 xl:grid-cols-3 2xl:grid-cols-4"><ChannelCard v-for="row in section.rows" :key="`${row.platform}:${row.group_id}`" :row="row" :category="section.category" :coverage="coverage" :is-admin="isAdmin" :multiplier="rates[row.group_id!]" /></div>
    </section>
    <div v-if="!loading && !sections.length" class="empty-state rounded-3xl border border-dashed border-gray-200 py-16 dark:border-dark-700"><p class="empty-state-title">{{ t('channelMonitorV2.cards.empty') }}</p><p class="empty-state-description">{{ t('channelMonitorV2.empty.description') }}</p><button v-if="search || attentionOnly || selectedCategory !== 'all'" type="button" class="btn btn-secondary btn-sm mt-4" @click="search = ''; attentionOnly = false; selectedCategory = 'all'">{{ t('channelMonitorV2.clearFilters') }}</button></div>
    <p v-if="isAdmin" class="text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('channelMonitorV2.cards.metricHint') }}</p>

    <DisplayOrderDialog v-if="isAdmin" :show="showOrder" :loading="orderLoading" :ready="orderConfig !== null" :saving="orderSaving" :order="orderConfig?.display_order" :groups="displayGroups" :initial-category="initialCategory" :error="orderError" @close="showOrder = false" @save="saveOrder" />
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { MonitorConfig, MonitorCoverage, MonitorDisplayOrder, MonitorMatrixRow } from '@/api/channelMonitorV2'
import { getConfig, updateConfig } from '@/api/channelMonitorV2'
import userGroupsAPI from '@/api/groups'
import Icon from '@/components/icons/Icon.vue'
import ProviderIcon from '@/components/user/monitor/ProviderIcon.vue'
import { useAppStore } from '@/stores/app'
import { extractApiErrorMessage } from '@/utils/apiError'
import ChannelCard from './ChannelCard.vue'
import DisplayOrderDialog from './DisplayOrderDialog.vue'
import { cardHealth, categoryOrder, orderedGroups, rowGroup, type DisplayCategory, type DisplayGroup } from './display'

const props = defineProps<{ rows: MonitorMatrixRow[]; coverage: MonitorCoverage | null; config: MonitorConfig | null; groups: DisplayGroup[]; isAdmin: boolean; loading: boolean }>()
const emit = defineEmits<{ saved: [config: MonitorConfig] }>()
const { t } = useI18n()
const appStore = useAppStore()
const selectedCategory = ref<DisplayCategory | 'all'>('all')
const search = ref('')
const attentionOnly = ref(false)
const rates = ref<Record<number, number>>({})
const rateLoadFailed = ref(false)
const showOrder = ref(false)
const orderLoading = ref(false)
const orderSaving = ref(false)
const orderError = ref('')
const orderConfig = ref<MonitorConfig | null>(null)
const initialCategory = ref<DisplayCategory>()
let rateSequence = 0
let orderSequence = 0
const displayGroups = computed(() => {
  const groups = new Map(props.groups.map(g => [g.id, g]))
  for (const row of props.rows) { if (row.group_id && !groups.has(row.group_id)) groups.set(row.group_id, rowGroup(row)) }
  return [...groups.values()]
})
const categories = computed(() => categoryOrder(props.config?.display_order).filter(category => rowsFor(category).length > 0))
watch(categories, visible => {
  if (selectedCategory.value !== 'all' && !visible.includes(selectedCategory.value)) selectedCategory.value = 'all'
})
const categoryLabel = (category: DisplayCategory) => t(`channelMonitorV2.cards.categories.${category}`)
function rowsFor(category: DisplayCategory) {
  const decorated = props.rows.map(row => ({ ...rowGroup(row), row }))
  return orderedGroups(decorated, category, props.config?.display_order).map(value => value.row)
}
const sections = computed(() => categories.value.filter(category => selectedCategory.value === 'all' || selectedCategory.value === category).map(category => ({
  category,
  rows: rowsFor(category).filter(row => (!search.value.trim() || (row.group_name || '').toLowerCase().includes(search.value.trim().toLowerCase())) && (!(props.isAdmin && attentionOnly.value) || ['warning', 'critical'].includes(cardHealth(row.metrics, row.health)))),
})).filter(section => section.rows.length))

async function loadRates() {
  const id = ++rateSequence
  // Treat custom rates as authoritative, including a legitimate zero multiplier.
  try {
    const [groups, overrides] = await Promise.all([userGroupsAPI.getAvailable(), userGroupsAPI.getUserGroupRates()])
    if (id !== rateSequence) return
    rates.value = Object.fromEntries(groups.map(group => [group.id, overrides[group.id] ?? group.rate_multiplier]))
    rateLoadFailed.value = false
  } catch {
    if (id !== rateSequence) return
    rates.value = {}
    rateLoadFailed.value = true
  }
}
async function openOrder(category?: DisplayCategory) {
  if (!props.isAdmin) return
  const id = ++orderSequence
  initialCategory.value = category
  orderLoading.value = true
  orderError.value = ''
  orderConfig.value = null
  showOrder.value = true
  try { const config = await getConfig(); if (id === orderSequence) orderConfig.value = config }
  catch (error) { if (id === orderSequence) orderError.value = extractApiErrorMessage(error, t('channelMonitorV2.cards.orderLoadFailed')) }
  finally { if (id === orderSequence) orderLoading.value = false }
}
async function saveOrder(order: MonitorDisplayOrder) {
  if (!props.isAdmin || !orderConfig.value || orderSaving.value) return
  const id = orderSequence
  orderSaving.value = true
  orderError.value = ''
  try {
    const config = await updateConfig({ ...orderConfig.value, display_order: order })
    if (id !== orderSequence || !props.isAdmin) return
    orderConfig.value = config
    emit('saved', config)
    showOrder.value = false
    appStore.showSuccess(t('channelMonitorV2.cards.orderSaved'))
  } catch (error) {
    if (id !== orderSequence || !props.isAdmin) return
    orderError.value = extractApiErrorMessage(error, t('channelMonitorV2.cards.orderSaveFailed'))
    // Retain the operator's draft on conflict. Reopening explicitly fetches the latest version.
  } finally { orderSaving.value = false }
}
watch(() => props.isAdmin, isAdmin => {
  if (isAdmin) return
  attentionOnly.value = false
  orderSequence++
  showOrder.value = false
  orderConfig.value = null
  orderError.value = ''
})
watch(() => props.coverage?.computed_at, () => { void loadRates() }, { immediate: true })
onBeforeUnmount(() => { rateSequence++; orderSequence++ })
</script>

<style scoped>
.category-tab{display:inline-flex;align-items:center;gap:7px;padding:9px 12px;border-radius:10px;font-size:12px;color:#64748b;border:1px solid transparent;transition:background .15s,border-color .15s}.category-tab span{font-size:10px;opacity:.85;font-variant-numeric:tabular-nums}.category-tab:hover{background:#f1f5f9}.category-tab.selected{background:rgb(var(--color-primary-50-rgb,255 247 228));color:rgb(var(--color-primary-700-rgb,135 95 19));border-color:rgb(var(--color-primary-200-rgb,241 223 180));font-weight:600}.category-tab:focus-visible{outline:2px solid rgb(var(--color-primary-500-rgb,197 150 66));outline-offset:2px}.legend-dot{width:6px;height:6px;border-radius:50%;display:inline-block}.legend-dot.state-healthy{background:#21b88c}.legend-dot.state-warning{background:#e5ab26}.legend-dot.state-critical{background:#eb6678}.legend-dot.state-unknown{background:#94a3b8}.section-provider{display:grid;place-items:center;width:36px;height:36px;border-radius:12px;background:#f1f5f9;color:#475569}.domestic-icons{display:flex;padding-right:1px}.domestic-icons>span{display:grid;place-items:center;width:32px;height:32px;border-radius:50%;border:2px solid #f8fafc;background:#e8effb;color:#496cbd}.domestic-icons>span+span{margin-left:-7px}.domestic-icons>span:nth-child(2){background:#f8eaf1;color:#a85f88}.domestic-icons>span:nth-child(3){background:#ececfa;color:#6c66b6}.dark .category-tab{color:#a5b3c6}.dark .category-tab:hover{background:#1e293b}.dark .category-tab.selected{background:rgb(200 153 61 / .12);border-color:rgb(202 157 65 / .23);color:#efd17f}.dark .section-provider{background:rgb(71 85 105 / .3);color:#cbd5e1}.dark .domestic-icons>span{border-color:#0f172a;background:#223254;color:#a6bfff}.dark .domestic-icons>span:nth-child(2){background:#34253c;color:#d9a4ca}.dark .domestic-icons>span:nth-child(3){background:#302d4d;color:#bbb5ed}@media(prefers-reduced-motion:reduce){.category-tab{transition:none}}
</style>
