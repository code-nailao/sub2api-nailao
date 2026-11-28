<template>
  <div class="space-y-5">
    <!-- 页头（独立形态下展示标题；后台形态 AppHeader 已有页面标题） -->
    <div v-if="!embedded">
      <h1 class="text-2xl font-bold tracking-tight text-gray-900 dark:text-white sm:text-3xl">
        {{ t('modelPlaza.title') }}
      </h1>
      <p class="mt-1.5 text-sm text-gray-500 dark:text-dark-400">{{ t('modelPlaza.description') }}</p>
    </div>

    <!-- 全局价格说明（管理员配置，Markdown） -->
    <div
      v-if="descriptionHtml"
      class="plaza-description rounded-2xl border border-gray-100 bg-white px-5 py-4 text-sm shadow-card dark:border-dark-700/50 dark:bg-dark-800/50"
      v-html="descriptionHtml"
    ></div>

    <!-- 未登录提示 -->
    <p v-if="!isAuthenticated" class="flex items-center gap-1.5 text-xs text-gray-400 dark:text-dark-500">
      <Icon name="infoCircle" size="xs" class="h-3.5 w-3.5" />
      {{ t('modelPlaza.anonymousHint') }}
    </p>

    <!-- 加载 / 错误 / 空 -->
    <div v-if="loading" class="flex min-h-[240px] items-center justify-center">
      <div class="h-8 w-8 animate-spin rounded-full border-2 border-primary-600/25 border-t-primary-600 dark:border-primary-400/25 dark:border-t-primary-400"></div>
    </div>
    <div
      v-else-if="error"
      class="rounded-2xl border border-red-200 bg-red-50 px-5 py-8 text-center text-sm text-red-600 dark:border-red-500/30 dark:bg-red-500/10 dark:text-red-300"
    >
      {{ t('modelPlaza.loadFailed') }}
    </div>
    <template v-else>
      <PlazaToolbar
        :platforms="platformTabs"
        :groups="groups"
        :platform="selectedPlatform"
        :group-id="selectedGroupId"
        :search="searchQuery"
        :model-count="filteredRows.length"
        :total-models="allRows.length"
        :group="selectedGroup"
        :rows="filteredRows"
        :all-rows="allRows"
        @update:platform="onPlatformChange"
        @update:group-id="onGroupChange"
        @update:search="searchQuery = $event"
      />

      <!-- 价格口径说明：实付价的来源，避免被读成汇率换算 -->
      <p class="flex items-start gap-1.5 text-xs leading-5 text-gray-400 dark:text-dark-500">
        <Icon name="infoCircle" size="xs" class="mt-0.5 h-3.5 w-3.5 shrink-0" />
        {{ t('modelPlaza.priceNote') }}
      </p>

      <!-- 按平台分节的模型清单：一个模型只出现一次 -->
      <div v-if="visibleSections.length > 0" class="space-y-5">
        <section
          v-for="section in visibleSections"
          :key="section.platform"
          class="overflow-hidden rounded-2xl border bg-white shadow-card dark:bg-dark-800/50"
          :class="platformBorderStrongClass(section.platform)"
        >
          <header class="flex flex-wrap items-center gap-2 border-b border-gray-100 px-5 py-3 dark:border-dark-700/60">
            <PlatformIcon
              v-if="section.platform !== CN_SEGMENT"
              :platform="section.platform as GroupPlatform"
              size="sm"
            />
            <span
              v-else
              class="flex h-6 w-6 items-center justify-center rounded-md bg-emerald-500/10 text-emerald-600 dark:text-emerald-400"
            >
              <Icon name="cube" size="sm" />
            </span>
            <h2 class="text-base font-semibold text-gray-900 dark:text-white">
              {{ segmentLabel(section.platform) }}
            </h2>
            <span class="text-xs text-gray-400 dark:text-dark-500">
              {{ t('modelPlaza.section.models', { count: section.rows.length }) }}
            </span>
          </header>
          <PlazaModelTable
            :rows="section.rows"
            :show-groups="isAllGroups"
            :is-group-mode="!isAllGroups"
            @select-group="onGroupChange"
          />
        </section>
      </div>
      <div
        v-else
        class="rounded-2xl border border-dashed border-gray-300 px-5 py-12 text-center text-sm text-gray-500 dark:border-dark-600 dark:text-dark-400"
      >
        {{ searchActive ? t('modelPlaza.noSearchResult') : t('modelPlaza.empty') }}
      </div>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { marked } from 'marked'
import DOMPurify from 'dompurify'
import Icon from '@/components/icons/Icon.vue'
import PlatformIcon from '@/components/common/PlatformIcon.vue'
import PlazaToolbar from './PlazaToolbar.vue'
import PlazaModelTable from './PlazaModelTable.vue'
import {
  buildPlazaRows,
  CN_SEGMENT,
  groupSegment,
  rowSegment,
  sortPlatforms,
  type PlazaRow
} from './plazaDisplay'
import type { ModelPlazaGroup, ModelPlazaResponse } from '@/api/modelPlaza'
import { useAuthStore } from '@/stores/auth'
import { platformBorderStrongClass, platformLabel } from '@/utils/platformColors'
import type { GroupPlatform } from '@/types'

const props = defineProps<{
  response: ModelPlazaResponse | null
  loading: boolean
  error?: boolean
  /** 后台内嵌形态（AppLayout 内）：隐藏页头。 */
  embedded?: boolean
}>()

const { t } = useI18n()
const authStore = useAuthStore()
const isAuthenticated = computed(() => authStore.isAuthenticated)

const selectedPlatform = ref<string>('all')
const selectedGroupId = ref<number | 'all'>('all')
const searchQuery = ref('')

const searchActive = computed(() => searchQuery.value.trim() !== '')
const groups = computed(() => props.response?.groups ?? [])
const isAllGroups = computed(() => selectedGroupId.value === 'all')
const selectedGroup = computed<ModelPlazaGroup | null>(() =>
  isAllGroups.value ? null : (groups.value.find((g) => g.id === selectedGroupId.value) ?? null)
)

const descriptionHtml = computed(() => {
  const md = props.response?.description?.trim()
  if (!md) return ''
  return DOMPurify.sanitize(marked.parse(md) as string)
})

/** 未按平台、搜索过滤的行：用于计数与全量导出。 */
const allRows = computed(() => buildPlazaRows(groups.value, 'all'))

const segments = computed(() => [...new Set(allRows.value.map(rowSegment))])

const platformTabs = computed(() => {
  const counts = new Map<string, number>()
  for (const row of allRows.value) {
    const segment = rowSegment(row)
    counts.set(segment, (counts.get(segment) ?? 0) + 1)
  }
  return sortPlatforms(segments.value).map((platform) => ({
    platform,
    count: counts.get(platform) ?? 0
  }))
})

/** 选定分组时只保留该分组的模型。 */
const rows = computed(() => buildPlazaRows(groups.value, selectedGroupId.value))

const filteredRows = computed(() => {
  let list = rows.value
  if (selectedPlatform.value !== 'all') {
    list = list.filter((row) => rowSegment(row) === selectedPlatform.value)
  }
  const q = searchQuery.value.trim().toLowerCase()
  if (q) {
    list = list.filter((row) => row.name.toLowerCase().includes(q))
  }
  return list
})

const visibleSections = computed(() => {
  const bySegment = new Map<string, PlazaRow[]>()
  for (const row of filteredRows.value) {
    const segment = rowSegment(row)
    const list = bySegment.get(segment) ?? []
    list.push(row)
    bySegment.set(segment, list)
  }
  return sortPlatforms([...bySegment.keys()]).map((platform) => ({
    platform,
    rows: bySegment.get(platform) ?? []
  }))
})

/** 国模集合单独命名，其余用平台显示名。 */
function segmentLabel(segment: string): string {
  return segment === CN_SEGMENT ? t('modelPlaza.segments.cn') : platformLabel(segment)
}

/** 选中分组时切到它所在的归集；切换平台时清掉不再匹配的分组。 */
function onGroupChange(value: number | 'all') {
  selectedGroupId.value = value
  if (value !== 'all') {
    const group = groups.value.find((g) => g.id === value)
    if (group) selectedPlatform.value = groupSegment(group)
  }
}

function onPlatformChange(value: string) {
  selectedPlatform.value = value
  if (selectedGroupId.value !== 'all') {
    const group = selectedGroup.value
    if (!group || (value !== 'all' && groupSegment(group) !== value)) {
      selectedGroupId.value = 'all'
    }
  }
}

/** 数据刷新后分组或平台可能不存在，回落到全部。 */
watch(groups, (list) => {
  if (selectedGroupId.value !== 'all' && !list.some((g) => g.id === selectedGroupId.value)) {
    selectedGroupId.value = 'all'
  }
  if (selectedPlatform.value !== 'all' && !segments.value.includes(selectedPlatform.value)) {
    selectedPlatform.value = 'all'
  }
})
</script>

<style scoped>
.plaza-description {
  line-height: 1.7;
  overflow-wrap: anywhere;
}

.plaza-description :deep(h1),
.plaza-description :deep(h2),
.plaza-description :deep(h3) {
  @apply mb-2 mt-3 font-semibold text-gray-900 first:mt-0 dark:text-white;
}

.plaza-description :deep(p) {
  @apply mb-2 text-gray-700 last:mb-0 dark:text-dark-200;
}

.plaza-description :deep(a) {
  @apply text-primary-600 underline underline-offset-4 hover:text-primary-700 dark:text-primary-300;
}

.plaza-description :deep(ul) {
  @apply mb-2 list-disc pl-5;
}

.plaza-description :deep(ol) {
  @apply mb-2 list-decimal pl-5;
}

.plaza-description :deep(li) {
  @apply mb-0.5 text-gray-700 dark:text-dark-200;
}

.plaza-description :deep(code) {
  @apply rounded bg-gray-100 px-1.5 py-0.5 font-mono text-xs dark:bg-dark-800;
}

.plaza-description :deep(blockquote) {
  @apply my-2 border-l-4 border-gray-300 pl-3 text-gray-600 dark:border-dark-600 dark:text-dark-300;
}
</style>
