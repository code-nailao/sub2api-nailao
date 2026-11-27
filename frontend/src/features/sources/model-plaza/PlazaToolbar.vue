<template>
  <div class="space-y-4 rounded-2xl border border-gray-200/70 bg-white p-4 shadow-sm dark:border-dark-700/60 dark:bg-dark-800/50">
    <!-- 平台：一条胶囊轨道，选中项用主色浅底 + 描边，计数放在右侧 -->
    <div class="flex flex-wrap items-center gap-1 rounded-2xl bg-gray-50/90 p-1.5 ring-1 ring-inset ring-gray-200/60 dark:bg-dark-900/40 dark:ring-dark-700/60">
      <button
        type="button"
        class="segment"
        :class="pillClass(platform === 'all')"
        @click="$emit('update:platform', 'all')"
      >
        {{ t('modelPlaza.filters.all') }}
        <span class="count" :class="{ 'count-active': platform === 'all' }">{{ totalModels }}</span>
      </button>
      <button
        v-for="item in platforms"
        :key="item.platform"
        type="button"
        class="segment"
        :class="pillClass(platform === item.platform)"
        @click="$emit('update:platform', item.platform)"
      >
        <span
          v-if="item.platform === CN_SEGMENT"
          class="flex h-4 w-4 items-center justify-center text-gray-500 dark:text-dark-300"
        >
          <Icon name="cube" size="xs" class="h-4 w-4" />
        </span>
        <PlatformIcon v-else :platform="item.platform as GroupPlatform" size="xs" />
        {{ segmentLabel(item.platform) }}
        <span class="count" :class="{ 'count-active': platform === item.platform }">{{ item.count }}</span>
      </button>
    </div>

    <!-- 分组倍率：价格的计算口径，默认「全部 · 最低价」 -->
    <div class="flex items-start gap-2">
      <span class="shrink-0 pt-1.5 text-[11px] font-semibold uppercase tracking-wider text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.groupLabel') }}
      </span>
      <div class="flex min-w-0 flex-1 flex-wrap items-center gap-1.5">
        <button type="button" class="pill" :class="pillClass(groupId === 'all')" @click="$emit('update:groupId', 'all')">
          {{ t('modelPlaza.filters.all') }}
          <span class="text-[11px] font-normal opacity-70">{{ t('modelPlaza.filters.lowestSuffix') }}</span>
        </button>
        <button
          v-for="group in visibleGroups"
          :key="group.id"
          type="button"
          class="pill max-w-[16rem]"
          :class="pillClass(groupId === group.id)"
          @click="$emit('update:groupId', group.id)"
        >
          <span class="min-w-0 truncate">{{ group.name }}</span>
          <span class="font-mono text-[11px] opacity-75">{{ effectiveRate(group) }}x</span>
        </button>
        <button
          v-if="hiddenGroupCount > 0 || groupsExpanded"
          type="button"
          class="rounded-xl px-3 py-1.5 text-[13px] font-medium text-gray-500 transition hover:bg-gray-50 hover:text-gray-900 dark:text-dark-400 dark:hover:bg-dark-700/60 dark:hover:text-white"
          @click="toggleGroups"
        >
          {{ groupsExpanded ? t('modelPlaza.toolbar.collapseGroups') : t('modelPlaza.toolbar.moreGroups', { count: hiddenGroupCount }) }}
        </button>
      </div>
    </div>

    <!-- 搜索、计数与导出（导出做成低存在感的图标，不影响主视觉） -->
    <div class="flex flex-wrap items-center justify-between gap-3">
      <p class="text-xs text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.toolbar.summary', { models: modelCount, groups: groups.length }) }}
      </p>
      <div class="flex items-center gap-1.5">
        <div class="relative w-full sm:w-72">
          <Icon name="search" size="sm" class="absolute left-3 top-1/2 -translate-y-1/2 text-gray-400 dark:text-dark-500" />
          <input
            :value="search"
            type="text"
            :placeholder="t('modelPlaza.filters.searchPlaceholder')"
            class="input rounded-full py-1.5 pl-9 pr-9 text-[13px]"
            @input="$emit('update:search', ($event.target as HTMLInputElement).value)"
          />
          <button
            v-if="search"
            type="button"
            class="absolute right-2.5 top-1/2 -translate-y-1/2 text-gray-400 transition-colors hover:text-gray-600 dark:text-dark-500 dark:hover:text-gray-300"
            @click="$emit('update:search', '')"
          >
            <Icon name="x" size="xs" class="h-3.5 w-3.5" />
          </button>
        </div>

        <details ref="exportMenu" class="group relative">
          <summary
            class="inline-flex h-8 w-8 cursor-pointer list-none items-center justify-center rounded-full text-gray-400 transition-colors hover:bg-gray-100 hover:text-gray-600 dark:text-dark-500 dark:hover:bg-dark-700/60 dark:hover:text-dark-200 [&::-webkit-details-marker]:hidden"
            :title="t('modelPlaza.export.title')"
            :aria-label="t('modelPlaza.export.title')"
          >
            <Icon name="download" size="sm" />
          </summary>
          <div class="absolute right-0 z-30 mt-1 w-72 rounded-xl border border-gray-200 bg-white p-1.5 shadow-lg dark:border-dark-700 dark:bg-dark-800">
            <button
              type="button"
              class="flex w-full items-center gap-2 rounded-lg px-2.5 py-2 text-left text-xs text-gray-700 transition-colors hover:bg-gray-50 dark:text-dark-200 dark:hover:bg-dark-700/60"
              @click="exportCsv('view')"
            >
              <Icon name="download" size="xs" class="h-3.5 w-3.5 text-gray-400 dark:text-dark-500" />
              {{ t('modelPlaza.export.view') }}
            </button>
            <button
              type="button"
              class="flex w-full items-center gap-2 rounded-lg px-2.5 py-2 text-left text-xs text-gray-700 transition-colors hover:bg-gray-50 dark:text-dark-200 dark:hover:bg-dark-700/60"
              @click="exportCsv('all-groups')"
            >
              <Icon name="download" size="xs" class="h-3.5 w-3.5 text-gray-400 dark:text-dark-500" />
              {{ t('modelPlaza.export.allGroups') }}
            </button>
            <button
              type="button"
              class="flex w-full items-center gap-2 rounded-lg px-2.5 py-2 text-left text-xs text-gray-700 transition-colors hover:bg-gray-50 dark:text-dark-200 dark:hover:bg-dark-700/60"
              @click="copyApi"
            >
              <Icon name="link" size="xs" class="h-3.5 w-3.5 text-gray-400 dark:text-dark-500" />
              {{ t('modelPlaza.export.copyApi') }}
            </button>
            <p class="mt-1 border-t border-gray-100 px-2.5 pb-1 pt-2 font-mono text-[10px] leading-4 text-gray-400 dark:border-dark-700 dark:text-dark-500">
              GET {{ apiUrl }}
            </p>
          </div>
        </details>
      </div>
    </div>

    <!-- 选中分组后的口径说明：倍率、高峰、生图独立倍率 -->
    <div
      v-if="group"
      class="rounded-xl bg-gray-50 px-4 py-3 text-xs text-gray-600 dark:bg-dark-800/70 dark:text-dark-300"
    >
      <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5">
        <span class="font-semibold text-gray-900 dark:text-white">{{ group.name }}</span>
        <span class="rounded-full bg-emerald-500/10 px-2 py-0.5 font-mono font-semibold text-emerald-700 dark:text-emerald-300">
          ×{{ effectiveRate(group) }}
        </span>
        <span v-if="savedPercent" class="text-emerald-600 dark:text-emerald-400">
          {{ t('modelPlaza.toolbar.saved', { percent: savedPercent }) }}
        </span>
        <span v-if="group.user_rate_multiplier != null && group.user_rate_multiplier !== group.rate_multiplier" class="text-amber-600 dark:text-amber-400">
          {{ t('modelPlaza.toolbar.personalRate', { rate: group.rate_multiplier }) }}
        </span>
        <span v-if="group.subscription_type === 'subscription'" class="rounded-md bg-violet-50 px-1.5 py-0.5 font-medium text-violet-600 dark:bg-violet-900/20 dark:text-violet-400">
          {{ t('modelPlaza.badges.subscription') }}
        </span>
        <span v-if="group.is_exclusive" class="rounded-md bg-purple-50 px-1.5 py-0.5 font-medium text-purple-600 dark:bg-purple-900/20 dark:text-purple-400">
          {{ t('modelPlaza.badges.exclusive') }}
        </span>
        <span v-if="peakWindow" class="text-amber-600 dark:text-amber-400">
          {{ t('modelPlaza.detail.peakNote', { window: peakWindow, multiplier: group.peak_rate_multiplier }) }}
        </span>
        <span v-if="group.image_rate_independent">
          {{ t('modelPlaza.toolbar.imageRate', { rate: group.image_rate_multiplier }) }}
        </span>
        <span v-if="group.video_rate_independent">
          {{ t('modelPlaza.toolbar.videoRate', { rate: group.video_rate_multiplier }) }}
        </span>
        <span v-if="group.long_context_pricing_enabled === false" class="text-gray-500 dark:text-dark-400">
          {{ t('modelPlaza.detail.longContextDisabledNote') }}
        </span>
      </div>
      <p v-if="group.description" class="mt-1.5 text-gray-500 dark:text-dark-400">{{ group.description }}</p>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import { platformLabel } from '@/utils/platformColors'
import { hasPeakRate, formatPeakRateWindow, serverTimezoneLabel } from '@/utils/peak-rate'
import { useAppStore } from '@/stores/app'
import type { ModelPlazaGroup } from '@/api/modelPlaza'
import type { GroupPlatform } from '@/types'
import { buildPlazaCsv, downloadCsv, plazaApiUrl } from './plazaExport'
import { useClipboard } from '@/composables/useClipboard'
import { CN_SEGMENT, effectiveRate, groupSegment, type PlazaRow } from './plazaDisplay'

const props = defineProps<{
  platforms: Array<{ platform: string; count: number }>
  groups: ModelPlazaGroup[]
  platform: string
  groupId: number | 'all'
  search: string
  modelCount: number
  totalModels: number
  group: ModelPlazaGroup | null
  /** 当前筛选后的行（导出「当前视图」用）。 */
  rows: PlazaRow[]
  /** 全量行（导出「全部模型 × 分组」用）。 */
  allRows: PlazaRow[]
}>()

defineEmits<{
  'update:platform': [value: string]
  'update:groupId': [value: number | 'all']
  'update:search': [value: string]
}>()

const { t } = useI18n()
const appStore = useAppStore()
const { copyToClipboard } = useClipboard()
const exportMenu = ref<HTMLDetailsElement | null>(null)
const apiUrl = plazaApiUrl()

/** 分组条默认只展示倍率最低的几个，避免几十个分组把页面撑乱。 */
const GROUP_PREVIEW_LIMIT = 6
const groupsExpanded = ref(false)

/** 分组按「归集」过滤：国模集合下面要看到【国模】1、【国模】2 这些分组。 */
const filteredGroups = computed(() =>
  props.platform === 'all'
    ? props.groups
    : props.groups.filter((g) => groupSegment(g) === props.platform)
)

const visibleGroups = computed(() => {
  const list = filteredGroups.value
  if (groupsExpanded.value || list.length <= GROUP_PREVIEW_LIMIT) return list
  const preview = list.slice(0, GROUP_PREVIEW_LIMIT)
  const selected = list.find((g) => g.id === props.groupId)
  // 选中的分组不在预览里时补到最前面，保证当前口径始终可见。
  return selected && !preview.includes(selected) ? [selected, ...preview] : preview
})

const hiddenGroupCount = computed(() =>
  groupsExpanded.value ? 0 : Math.max(0, filteredGroups.value.length - visibleGroups.value.length)
)

/** 倍率语义换算：0.035x 即按官方价 3.5 折，省 96.5%。 */
const savedPercent = computed(() => {
  if (!props.group) return ''
  const rate = effectiveRate(props.group)
  if (!(rate > 0) || rate >= 1) return ''
  return String(Math.round((1 - rate) * 1000) / 10)
})

const peakWindow = computed(() => {
  if (!props.group || !hasPeakRate(props.group)) return ''
  return formatPeakRateWindow(
    props.group,
    serverTimezoneLabel(appStore.cachedPublicSettings?.server_utc_offset)
  )
})

function segmentLabel(segment: string): string {
  return segment === CN_SEGMENT ? t('modelPlaza.segments.cn') : platformLabel(segment)
}

function pillClass(active: boolean): string {
  return active
    ? 'bg-primary-50 text-primary-700 ring-1 ring-inset ring-primary-300/70 shadow-sm dark:bg-primary-500/15 dark:text-primary-200 dark:ring-primary-500/40'
    : 'text-gray-600 hover:bg-white hover:text-gray-900 dark:text-dark-300 dark:hover:bg-dark-700 dark:hover:text-white'
}

function toggleGroups() {
  groupsExpanded.value = !groupsExpanded.value
}

watch(
  () => props.platform,
  () => {
    groupsExpanded.value = false
  }
)

/** 导出当前口径或全量分组明细；导出后收起菜单，保持页面干净。 */
function exportCsv(mode: 'view' | 'all-groups') {
  const rows = mode === 'all-groups' ? props.allRows : props.rows
  const stamp = new Date().toISOString().slice(0, 10)
  const suffix = mode === 'all-groups' ? 'all-groups' : (props.group?.name ?? props.platform)
  downloadCsv(`model-plaza-${suffix}-${stamp}.csv`, buildPlazaCsv(rows, mode))
  closeMenu()
}

async function copyApi() {
  await copyToClipboard(apiUrl, t('modelPlaza.export.copied'))
  closeMenu()
}

function closeMenu() {
  if (exportMenu.value) exportMenu.value.open = false
}
</script>

<style scoped>
.pill {
  @apply inline-flex shrink-0 items-center gap-1.5 rounded-xl px-3 py-1.5 text-[13px] font-medium transition;
}

.segment {
  @apply inline-flex shrink-0 items-center gap-2 rounded-[10px] px-3.5 py-1.5 text-[13px] font-medium transition;
}

.count {
  @apply rounded-md bg-gray-200/70 px-1.5 py-0.5 font-mono text-[11px] font-normal text-gray-500 dark:bg-dark-700 dark:text-dark-400;
}

.count-active {
  @apply bg-primary-100/80 text-primary-700 dark:bg-primary-500/20 dark:text-primary-200;
}
</style>
