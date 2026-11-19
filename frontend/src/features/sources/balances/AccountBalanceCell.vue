<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { formatDateTime, formatRelativeTime } from '@/utils/format'
import { compactAmount, decimalAmount, unlimitedAmount, visibleBalance } from './presentation'
import type { AccountBalance } from './types'

const props = defineProps<{ summary?: AccountBalance; supported: boolean; loading?: boolean; unavailable?: boolean }>()
defineEmits<{ configure: [] }>()
const { t } = useI18n()
const balance = computed(() => visibleBalance(props.summary))
const historical = computed(() => Boolean(balance.value && (props.unavailable || props.summary?.error || ['error', 'unit_unconfirmed'].includes(props.summary?.status ?? ''))))
const unlimited = computed(() => Boolean(balance.value && unlimitedAmount(balance.value.remaining)))
const title = computed(() => balance.value
  ? `${decimalAmount(balance.value.remaining)} ${balance.value.unit || t('sourcesBalance.unitUnknown')}\n${t('sourcesBalance.lastSuccess', { time: formatDateTime(balance.value.checked_at) })}${props.summary?.query_url ? `\n${t('sourcesBalance.queryOrigin')}: ${props.summary.query_url}` : ''}`
  : undefined)
const failureLabel = computed(() => props.unavailable ? t('sourcesBalance.unavailable')
  : props.summary?.error === 'upstream_timeout' ? t('sourcesBalance.queryTimeout')
  : props.summary?.error === 'refresh_in_progress_or_rate_limited' ? t('sourcesBalance.refreshBusy')
  : t('sourcesBalance.failed'))
const badgeClass = computed(() => {
  if (historical.value || props.summary?.status === 'low') return 'bg-yellow-100 text-yellow-700 dark:bg-yellow-900/30 dark:text-yellow-300'
  if (props.summary?.status === 'error' || props.unavailable) return 'bg-red-100 text-red-700 dark:bg-red-900/30 dark:text-red-300'
  if (balance.value) return 'bg-emerald-100 text-emerald-700 dark:bg-emerald-900/30 dark:text-emerald-300'
  if (props.loading || props.summary?.status === 'pending') return 'bg-blue-100 text-blue-700 dark:bg-blue-900/30 dark:text-blue-300'
  return 'bg-gray-100 text-gray-500 dark:bg-dark-700 dark:text-dark-300'
})
const label = computed(() => {
  if (props.summary?.querying && !balance.value) return t('sourcesBalance.refreshBusy')
  if (!props.supported || props.summary?.status === 'unsupported') return t('sourcesBalance.unsupported')
  if (props.summary?.status === 'rebind_required') return t('sourcesBalance.rebind')
  if (props.summary?.status === 'not_found') return t('sourcesBalance.missing')
  if (props.summary?.status === 'wallet_unavailable') return t('sourcesBalance.walletUnavailable')
  if (props.summary?.status === 'automatic_unavailable') return t('sourcesBalance.automaticUnavailable')
  if (props.summary?.status === 'pending' && !balance.value) return t('sourcesBalance.pending')
  if (balance.value) return unlimited.value ? t('sourcesBalance.unlimited') : compactAmount(balance.value.remaining)
  if (props.unavailable) return t('sourcesBalance.unavailable')
  if (props.loading && !props.summary) return t('sourcesBalance.loading')
  if (props.summary?.status === 'error') return failureLabel.value
  if (props.summary?.status === 'unit_unconfirmed') return t('sourcesBalance.unitUnknown')
  return t(props.summary?.source_id ? 'sourcesBalance.pending' : 'sourcesBalance.unbound')
})
</script>

<template>
  <span v-if="!supported || summary?.status === 'unsupported'" class="inline-flex w-fit items-center rounded-md bg-gray-100 px-2 py-0.5 text-xs font-medium text-gray-500 dark:bg-dark-700 dark:text-dark-300">{{ label }}</span>
  <button v-else type="button" class="group flex max-w-[220px] flex-col items-start gap-1 rounded-md text-left text-xs focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary-500"
    :title="title" :aria-label="t('sourcesBalance.configure')" data-testid="account-balance-cell" @click="$emit('configure')">
    <span class="flex flex-wrap items-center gap-1.5">
      <span class="inline-flex w-fit items-center gap-1 rounded-md px-2 py-0.5 font-medium tabular-nums" :class="badgeClass">
        <span>{{ label }}</span>
        <span v-if="balance && !unlimited">{{ balance.unit || t('sourcesBalance.unitUnknown') }}</span>
      </span>
      <span v-if="summary?.querying" class="text-[11px] text-blue-600 dark:text-blue-300">{{ t('sourcesBalance.refreshBusy') }}</span>
      <span v-if="summary?.status === 'low'" class="rounded bg-amber-50 px-1.5 py-0.5 text-[10px] text-amber-700 dark:bg-amber-900/30 dark:text-amber-300">{{ t('sourcesBalance.low') }}</span>
      <span v-if="historical" class="rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-normal text-gray-600 dark:bg-dark-700 dark:text-gray-400">{{ t('sourcesBalance.historical') }}</span>
      <span v-else-if="summary?.stale && balance" class="text-[10px] font-normal text-amber-700 dark:text-amber-300">{{ t('sourcesBalance.stale') }}</span>
    </span>
    <span v-if="balance?.checked_at" class="text-[11px] text-gray-400 dark:text-dark-400" :title="t('sourcesBalance.lastSuccess', { time: formatDateTime(balance.checked_at) })">
      {{ formatRelativeTime(balance.checked_at) }}<template v-if="(summary?.shared_count ?? 0) > 1"> · {{ t('sourcesBalance.shared', { count: summary?.shared_count }) }}</template>
    </span>
    <span v-if="historical" class="text-[11px] text-amber-700 dark:text-amber-300">{{ summary?.status === 'unit_unconfirmed' ? t('sourcesBalance.unitUnknown') : failureLabel }}</span>
  </button>
</template>
