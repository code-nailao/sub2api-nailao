<template>
  <div class="space-y-3">
    <!-- 平台：一眼看清在卖哪几家 -->
    <div class="flex flex-wrap items-center gap-1.5">
      <button
        type="button"
        class="rounded-lg px-3 py-1.5 text-sm font-medium transition"
        :class="chipClass(platform === 'all')"
        @click="$emit('update:platform', 'all')"
      >
        {{ t('modelPlaza.filters.all') }}
        <span class="ml-1 font-mono text-[11px] opacity-70">{{ totalModels }}</span>
      </button>
      <button
        v-for="item in platforms"
        :key="item.platform"
        type="button"
        class="inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-sm font-medium transition"
        :class="platform === item.platform ? 'chip-tinted-active' : 'chip-tinted'"
        :style="{ '--chip-accent': platformAccentColor(item.platform) }"
        @click="$emit('update:platform', item.platform)"
      >
        <PlatformIcon :platform="item.platform as GroupPlatform" size="xs" />
        {{ platformLabel(item.platform) }}
        <span class="font-mono text-[11px] opacity-70">{{ item.count }}</span>
      </button>
    </div>

    <!-- 分组倍率：价格的计算口径，默认「全部 · 最低价」 -->
    <div class="flex items-start gap-2">
      <span class="shrink-0 pt-1.5 text-xs font-semibold uppercase tracking-wider text-gray-400 dark:text-dark-500">
        {{ t('modelPlaza.filters.groupLabel') }}
      </span>
      <div class="flex min-w-0 flex-1 flex-wrap items-center gap-1.5">
        <button
          type="button"
          class="shrink-0 rounded-lg px-3 py-1.5 text-sm font-medium transition"
          :class="chipClass(groupId === 'all')"
          @click="$emit('update:groupId', 'all')"
        >
          {{ t('modelPlaza.filters.all') }}
          <span class="ml-1 text-[11px] font-normal opacity-75">{{ t('modelPlaza.filters.lowestSuffix') }}</span>
        </button>
        <button
          v-for="group in visibleGroups"
          :key="group.id"
          type="button"
          class="inline-flex items-center gap-1.5 rounded-lg px-3 py-1.5 text-sm font-medium transition"
          :class="groupId === group.id ? 'chip-tinted-active' : 'chip-tinted'"
          :style="{ '--chip-accent': platformAccentColor(group.platform) }"
          @click="$emit('update:groupId', group.id)"
        >
          <span class="max-w-[12rem] truncate">{{ group.name }}</span>
          <span class="font-mono text-[11px] opacity-80">{{ effectiveRate(group) }}x</span>
        </button>
        <button
          v-if="hiddenGroupCount > 0 || groupsExpanded"
          type="button"
          class="shrink-0 rounded-lg px-3 py-1.5 text-sm font-medium text-gray-500 ring-1 ring-inset ring-gray-200 transition hover:bg-gray-50 hover:text-gray-900 dark:text-dark-400 dark:ring-dark-700 dark:hover:bg-dark-800"
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
            class="input rounded-lg py-1.5 pl-9 pr-9"
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
            class="inline-flex h-8 w-8 cursor-pointer list-none items-center justify-center rounded-lg text-gray-400 transition-colors hover:bg-gray-100 hover:text-gray-600 dark:text-dark-500 dark:hover:bg-dark-700/60 dark:hover:text-dark-200 [&::-webkit-details-marker]:hidden"
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
      class="rounded-xl border border-gray-100 bg-gray-50/70 px-4 py-3 text-xs text-gray-600 dark:border-dark-700/60 dark:bg-dark-800/40 dark:text-dark-300"
    >
      <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5">
        <span class="font-semibold text-gray-900 dark:text-white">{{ group.name }}</span>
        <span class="rounded-md bg-primary-50 px-1.5 py-0.5 font-mono font-semibold text-primary-700 dark:bg-primary-900/20 dark:text-primary-300">
          {{ effectiveRate(group) }}x
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
import { platformAccentColor, platformLabel } from '@/utils/platformColors'
import { hasPeakRate, formatPeakRateWindow, serverTimezoneLabel } from '@/utils/peak-rate'
import { useAppStore } from '@/stores/app'
import type { ModelPlazaGroup } from '@/api/modelPlaza'
import type { GroupPlatform } from '@/types'
import { effectiveRate, type PlazaRow } from './plazaDisplay'
import { buildPlazaCsv, downloadCsv, plazaApiUrl } from './plazaExport'
import { useClipboard } from '@/composables/useClipboard'

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

const filteredGroups = computed(() =>
  props.platform === 'all' ? props.groups : props.groups.filter((g) => g.platform === props.platform)
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

const peakWindow = computed(() => {
  if (!props.group || !hasPeakRate(props.group)) return ''
  return formatPeakRateWindow(
    props.group,
    serverTimezoneLabel(appStore.cachedPublicSettings?.server_utc_offset)
  )
})

function chipClass(active: boolean): string {
  return active
    ? 'bg-gradient-to-r from-primary-500 to-primary-600 text-white shadow-sm shadow-primary-500/30'
    : 'bg-white text-gray-600 ring-1 ring-inset ring-gray-200 hover:bg-gray-50 hover:text-gray-900 hover:ring-gray-300 dark:bg-dark-800/60 dark:text-dark-300 dark:ring-dark-700 dark:hover:bg-dark-800 dark:hover:text-white'
}
</script>

<style scoped>
/* 横向滚动条在 macOS 上悬停才出现，这里直接隐藏，避免在分组条下方留一条横线。 */
.scrollbar-none {
  scrollbar-width: none;
}

.scrollbar-none::-webkit-scrollbar {
  display: none;
}

.chip-tinted {
  color: var(--chip-accent);
  color: color-mix(in srgb, var(--chip-accent) 78%, black);
  background-color: color-mix(in srgb, var(--chip-accent) 9%, transparent);
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--chip-accent) 25%, transparent);
}

.chip-tinted:hover {
  background-color: color-mix(in srgb, var(--chip-accent) 16%, transparent);
}

.dark .chip-tinted {
  color: color-mix(in srgb, var(--chip-accent) 72%, white);
  background-color: color-mix(in srgb, var(--chip-accent) 12%, transparent);
  box-shadow: inset 0 0 0 1px color-mix(in srgb, var(--chip-accent) 30%, transparent);
}

.dark .chip-tinted:hover {
  background-color: color-mix(in srgb, var(--chip-accent) 18%, transparent);
}

.chip-tinted-active {
  color: #fff;
  background-color: var(--chip-accent);
  background-color: color-mix(in srgb, var(--chip-accent) 85%, black);
  box-shadow: 0 1px 2px 0 color-mix(in srgb, var(--chip-accent) 35%, transparent);
}

.chip-tinted-active:hover {
  background-color: color-mix(in srgb, var(--chip-accent) 75%, black);
}

.dark .chip-tinted-active {
  background-color: color-mix(in srgb, var(--chip-accent) 80%, transparent);
}

.dark .chip-tinted-active:hover {
  background-color: var(--chip-accent);
}
</style>
