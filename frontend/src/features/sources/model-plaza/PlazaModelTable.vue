<template>
  <div class="overflow-x-auto">
    <table class="w-full min-w-[980px] table-auto border-collapse text-sm tabular-nums">
      <colgroup>
        <col class="w-[24%]" />
        <col class="w-[12%]" />
        <col class="w-[12%]" />
        <col class="w-[18%]" />
        <col class="w-[11%]" />
        <col class="w-[9%]" />
        <col v-if="showGroups" class="w-[14%]" />
      </colgroup>
      <thead>
        <tr class="text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-dark-400">
          <th rowspan="2" class="border-r border-gray-100 py-2.5 pl-5 pr-4 text-left align-middle dark:border-dark-700/60">
            {{ t('modelPlaza.table.model') }}
          </th>
          <th colspan="4" class="pt-2 text-center">
            <div class="border-b border-gray-200 pb-2 dark:border-dark-700">
              {{ t('modelPlaza.table.paidPrice') }}
              <span class="ml-1 font-normal normal-case text-gray-400 dark:text-dark-500">{{ t('modelPlaza.table.unitPerMillion') }}</span>
            </div>
          </th>
          <th rowspan="2" class="border-l border-gray-100 py-2.5 text-center align-middle dark:border-dark-700/60">
            {{ t('modelPlaza.table.rate') }}
          </th>
          <th v-if="showGroups" rowspan="2" class="border-l border-gray-100 py-2.5 pl-4 pr-5 text-left align-middle dark:border-dark-700/60">
            {{ t('modelPlaza.table.groups') }}
          </th>
        </tr>
        <tr class="border-b border-gray-200 text-center text-[11px] font-medium leading-4 tracking-wide text-gray-400 dark:border-dark-700 dark:text-dark-500">
          <th class="px-3 py-2 font-medium">{{ t('modelPlaza.table.input') }}</th>
          <th class="px-3 py-2 font-medium">{{ t('modelPlaza.table.output') }}</th>
          <th class="px-3 py-2 font-medium">{{ t('modelPlaza.table.cacheWrite') }}</th>
          <th class="px-3 py-2 font-medium">{{ t('modelPlaza.table.cacheRead') }}</th>
        </tr>
      </thead>
      <tbody>
        <tr
          v-for="row in sortedRows"
          :key="row.key"
          class="border-b border-gray-100 align-top transition-colors last:border-b-0 hover:bg-gray-50/70 dark:border-dark-800 dark:hover:bg-dark-800/50"
        >
          <!-- 模型名与徽章 -->
          <td class="border-r border-gray-100 py-3 pl-5 pr-4 dark:border-dark-700/60">
            <div class="flex flex-wrap items-center gap-1.5">
              <span class="inline-flex min-w-0 items-center gap-1">
                <span class="min-w-0 break-words font-medium text-gray-900 dark:text-white">{{ row.name }}</span>
                <button
                  type="button"
                  class="inline-flex h-6 w-6 shrink-0 items-center justify-center rounded text-gray-400 transition-colors hover:bg-gray-100 hover:text-primary-600 dark:text-dark-500 dark:hover:bg-dark-700 dark:hover:text-primary-400"
                  :title="t('modelPlaza.table.copyModelId', { model: row.name })"
                  :aria-label="t('modelPlaza.table.copyModelId', { model: row.name })"
                  @click="copyToClipboard(row.name)"
                >
                  <Icon name="copy" size="sm" aria-hidden="true" />
                </button>
              </span>
              <span
                v-if="billingMode(row.primary.model) !== BILLING_MODE_TOKEN"
                class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
              >
                {{ billingModeLabel(row.primary.model) }}
              </span>
              <span
                v-if="row.primary.model.long_context_basis === 'marginal'"
                class="rounded-md bg-gray-100 px-1.5 py-0.5 text-[10px] font-medium text-gray-500 dark:bg-dark-700/70 dark:text-dark-300"
                :title="t('modelPlaza.table.tierHintMarginal')"
              >
                {{ t('modelPlaza.table.marginalBadge') }}
              </span>
              <span
                v-for="[effort, multiplier] in reasoningEffortMultipliers(row.primary.model)"
                :key="effort"
                class="rounded-md bg-amber-50 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 dark:bg-amber-900/20 dark:text-amber-300"
                :title="t('modelPlaza.table.reasoningMultiplierHint', { effort, multiplier })"
              >
                {{ t('modelPlaza.table.reasoningMultiplierBadge', { effort, multiplier }) }}
              </span>
              <span
                v-for="period in timePeriods(row.primary.model)"
                :key="period.start_time"
                class="rounded-md bg-sky-50 px-1.5 py-0.5 font-mono text-[10px] font-medium text-sky-700 dark:bg-sky-900/20 dark:text-sky-300"
                :title="timePricingHint(row, period)"
              >
                {{ formatTimeWindow(period) }} ×{{ period.multiplier }}
              </span>
            </div>
          </td>

          <!-- token 计价：输入 / 输出 / 缓存写 / 缓存读，多档时每档一组 -->
          <template v-if="billingMode(row.primary.model) === BILLING_MODE_TOKEN">
            <td v-for="column in tokenColumns" :key="column.priceKey" class="px-3 py-3">
              <div
                v-for="(line, idx) in cellLines(row, column)"
                :key="idx"
                class="mb-1.5 last:mb-0"
              >
                <div class="whitespace-nowrap leading-5">
                  <span
                    v-if="line.label"
                    class="mr-1 font-sans text-[11px] font-normal text-gray-400 dark:text-dark-500"
                  >{{ line.label }}</span>
                  <span class="font-mono font-semibold text-gray-900 dark:text-gray-50">{{ line.paid }}</span>
                </div>
                <div
                  v-if="line.extra"
                  class="whitespace-nowrap font-mono text-[10px] leading-4 text-gray-400 dark:text-dark-500"
                >
                  {{ line.extra }}
                </div>
                <div
                  v-if="line.official"
                  class="whitespace-nowrap font-mono text-[10px] leading-4 text-gray-400 dark:text-dark-500"
                >
                  {{ t('modelPlaza.table.officialShort') }} {{ line.official }}
                </div>
              </div>
            </td>
          </template>

          <!-- 按次 / 按张计费：价格合并到实付分区 -->
          <td v-else colspan="4" class="px-3 py-3">
            <div class="flex flex-wrap items-baseline gap-x-2 gap-y-1">
              <span class="font-mono font-semibold text-gray-900 dark:text-gray-50">
                {{ paidRequestPrice(row.primary.model.pricing?.per_request_price ?? row.primary.model.pricing?.image_input_price, row.primary) }}
              </span>
              <span class="text-xs text-gray-400 dark:text-dark-500">{{ perUnitSuffix(row.primary.model) }}</span>
              <span v-if="row.official?.input_price != null" class="font-mono text-[10px] text-gray-400 dark:text-dark-500">
                {{ t('modelPlaza.table.officialShort') }} {{ officialPrice(row.official.input_price) }}
                <span class="font-sans">{{ t('modelPlaza.table.unitPerMillion') }}</span>
              </span>
            </div>
          </td>

          <!-- 倍率 -->
          <td class="border-l border-gray-100 px-2 py-3 text-center align-top font-mono text-xs dark:border-dark-700/60">
            <div v-if="row.primary.personal" class="text-gray-400 line-through dark:text-dark-500">
              {{ row.primary.group.rate_multiplier }}x
            </div>
            <div class="font-bold text-primary-600 dark:text-primary-400">{{ offeringRate(row.primary) }}x</div>
            <div v-if="!isGroupMode && row.offerings.length > 1" class="mt-0.5 font-sans text-[10px] font-normal text-gray-400 dark:text-dark-500">
              {{ t('modelPlaza.table.lowest') }}
            </div>
          </td>

          <!-- 可用分组：列出提供该模型的全部可见分组及各自倍率 -->
          <td v-if="showGroups" class="border-l border-gray-100 py-3 pl-4 pr-5 align-top dark:border-dark-700/60">
            <details class="group relative">
              <summary class="inline-flex cursor-pointer list-none items-center gap-1 rounded-md text-xs font-medium text-gray-500 transition-colors hover:text-primary-600 dark:text-dark-400 dark:hover:text-primary-400 [&::-webkit-details-marker]:hidden">
                {{ t('modelPlaza.table.groupCount', { count: row.offerings.length }) }}
                <Icon name="chevronDown" size="xs" class="h-3.5 w-3.5 transition-transform group-open:rotate-180" />
              </summary>
              <div class="absolute right-0 z-20 mt-1 w-64 rounded-xl border border-gray-200 bg-white p-2 shadow-lg dark:border-dark-700 dark:bg-dark-800">
                <button
                  v-for="offering in row.offerings"
                  :key="offering.group.id"
                  type="button"
                  class="flex w-full items-center justify-between gap-2 rounded-lg px-2 py-1.5 text-left text-xs transition-colors hover:bg-gray-50 dark:hover:bg-dark-700/60"
                  :title="t('modelPlaza.table.jumpToGroup', { group: offering.group.name })"
                  @click="selectGroup($event, offering.group.id)"
                >
                  <span class="min-w-0 truncate text-gray-700 dark:text-dark-200">{{ offering.group.name }}</span>
                  <span class="shrink-0 font-mono font-semibold text-primary-600 dark:text-primary-400">{{ offeringRate(offering) }}x</span>
                </button>
              </div>
            </details>
          </td>
        </tr>
      </tbody>
    </table>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'
import { useClipboard } from '@/composables/useClipboard'
import {
  BILLING_MODE_IMAGE,
  BILLING_MODE_TOKEN,
  REASONING_EFFORT_LEVELS
} from '@/constants/channel'
import type { PlazaModel, PlazaTimePricingPeriod } from '@/api/modelPlaza'
import type { UserPricingInterval } from '@/api/channels'
import {
  billingMode,
  formatTimeWindow,
  offeringRate,
  officialIntervals,
  officialPrice,
  paidPrice,
  paidRequestPrice,
  sortPlazaRows,
  tierLabel,
  tokenIntervals,
  type PlazaOffering,
  type PlazaRow
} from './plazaDisplay'

const props = defineProps<{
  rows: PlazaRow[]
  /** 是否展示「可用分组」列：按最低价展示时才有意义。 */
  showGroups?: boolean
  /** 当前是否选中了单个分组（选中时分组信息已在页头展示）。 */
  isGroupMode?: boolean
}>()

const emit = defineEmits<{
  /** 点「可用分组」里的某个分组：整表切到该分组的实付价。 */
  'select-group': [id: number]
}>()

const { t } = useI18n()
const { copyToClipboard } = useClipboard()

/** 选中分组后收起浮层。 */
function selectGroup(event: MouseEvent, id: number) {
  emit('select-group', id)
  const details = (event.currentTarget as HTMLElement)?.closest('details')
  if (details) details.open = false
}

type PriceKey = 'input_price' | 'output_price' | 'cache_write_price' | 'cache_read_price'

interface PriceColumn {
  priceKey: PriceKey
  officialKey: PriceKey
}

const tokenColumns: PriceColumn[] = [
  { priceKey: 'input_price', officialKey: 'input_price' },
  { priceKey: 'output_price', officialKey: 'output_price' },
  { priceKey: 'cache_write_price', officialKey: 'cache_write_price' },
  { priceKey: 'cache_read_price', officialKey: 'cache_read_price' }
]

/** 单元格内的一行：档位标签 + 实付价 + 附加价（1h/12h 缓存写入）+ 官方参考价。 */
interface CellLine {
  label: string
  paid: string
  extra: string
  official: string
}

const sortedRows = computed(() => sortPlazaRows(props.rows))

/** 同一档位的官方价：按 min_tokens 对齐，不按下标，避免两侧档位数量不一致时错位。 */
function officialAt(model: PlazaModel, iv: UserPricingInterval): UserPricingInterval | null {
  return officialIntervals(model).find((o) => o.min_tokens === iv.min_tokens) ?? null
}

/** 缓存写入的长时段单价：1h / 12h 只在配置了独立价格时展示。 */
function extraCache(
  source: { cache_write_1h_price?: number | null; cache_write_12h_price?: number | null } | null | undefined,
  offering: PlazaOffering
): string {
  if (!source) return ''
  const parts: string[] = []
  if (source.cache_write_1h_price != null) parts.push(`1h ${paidPrice(source.cache_write_1h_price, offering)}`)
  if (source.cache_write_12h_price != null) parts.push(`12h ${paidPrice(source.cache_write_12h_price, offering)}`)
  return parts.join(' · ')
}

function cellLines(row: PlazaRow, column: PriceColumn): CellLine[] {
  const model = row.primary.model
  const intervals = tokenIntervals(model)
  if (intervals.length > 0) {
    return intervals.map((iv) => {
      const official = officialAt(model, iv)
      return {
        // 档位标签只放在输入列，其余列空着即与同一档位对齐。
        label: column.priceKey === 'input_price' ? tierLabel(iv) : '',
        paid: paidPrice(iv[column.priceKey], row.primary),
        extra: column.priceKey === 'cache_write_price' ? extraCache(iv, row.primary) : '',
        official: official ? officialPrice(official[column.priceKey]) : ''
      }
    })
  }
  const pricing = model.pricing
  const official = row.official
  return [
    {
      label: '',
      paid: paidPrice(pricing?.[column.priceKey], row.primary),
      extra: column.priceKey === 'cache_write_price' ? extraCache(pricing, row.primary) : '',
      official: official?.[column.officialKey] != null ? officialPrice(official[column.officialKey]) : ''
    }
  ]
}

function billingModeLabel(model: PlazaModel): string {
  return billingMode(model) === BILLING_MODE_IMAGE
    ? t('modelPlaza.table.perImage')
    : t('modelPlaza.table.perRequest')
}

function perUnitSuffix(model: PlazaModel): string {
  return billingMode(model) === BILLING_MODE_IMAGE
    ? t('modelPlaza.table.perUnitImage')
    : t('modelPlaza.table.perUnitRequest')
}

function reasoningEffortMultipliers(model: PlazaModel): [string, number][] {
  const multipliers = model.pricing?.reasoning_effort_multipliers
  return REASONING_EFFORT_LEVELS.flatMap((effort) => {
    const multiplier = multipliers?.[effort]
    return typeof multiplier === 'number' && Number.isFinite(multiplier) && multiplier > 0
      ? [[effort, multiplier] as [string, number]]
      : []
  })
}

function timePeriods(model: PlazaModel): PlazaTimePricingPeriod[] {
  return model.time_pricing?.periods ?? []
}

function timePricingHint(row: PlazaRow, period: PlazaTimePricingPeriod): string {
  return t('modelPlaza.table.timePricingRowHint', {
    timezone: row.primary.model.time_pricing?.timezone ?? '',
    rate: offeringRate(row.primary),
    multiplier: period.multiplier
  })
}
</script>
