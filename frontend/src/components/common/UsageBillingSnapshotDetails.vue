<template>
  <div v-if="snapshot && (isRejectionSnapshot || snapshot.version === 1)" class="space-y-1 border-t border-gray-700 pt-2" data-testid="billing-snapshot">
    <template v-if="isRejectionSnapshot">
      <div class="flex items-center justify-between gap-6" data-testid="rejection-charge">
        <span class="text-gray-400">{{ t('upstreamRejection.title') }}</span>
        <span class="font-mono text-white">{{ fixedChargeText }}</span>
      </div>
      <p v-if="fixedChargeAmount !== null" class="text-gray-300">{{ t('upstreamRejection.hint', { price: fixedChargeAmount }) }}</p>
    </template>
    <template v-else-if="snapshot.deepseek_tools">
      <div v-for="row in deepSeekToolRows" :key="row.tool" class="flex justify-between gap-4" :data-testid="`deepseek-tool-${row.tool}`">
        <span>{{ t(`deepseekTools.${row.tool}`) }} × {{ row.units }}{{ row.unitLabel ? ` ${t(row.unitLabel)}` : '' }}</span>
        <span class="font-mono">{{ row.amount.toFixed(6) }}u</span>
      </div>
      <p class="text-xs text-gray-400">{{ t('deepseekTools.priceHint') }}</p>
    </template>
    <template v-else>
    <div v-for="row in tierRows" :key="row.label" class="flex items-center justify-between gap-6">
      <span class="text-gray-400">{{ row.label }}</span>
      <span class="font-mono text-white">{{ row.value }}</span>
    </div>
    <div v-if="snapshot.tier_multiplier" class="flex justify-between gap-6">
      <span class="text-gray-400">{{ t('usage.tierMultiplier') }}</span>
      <span class="text-cyan-300">{{ snapshot.tier_multiplier }}×</span>
    </div>
    <div v-if="snapshot.charged_tier_multiplier" class="flex justify-between gap-6">
      <span class="text-gray-400">{{ t('usage.chargedTierMultiplier') }}</span>
      <span class="text-green-400">{{ snapshot.charged_tier_multiplier }}×</span>
    </div>
    <div class="flex justify-between gap-6">
      <span class="text-gray-400">{{ t('usage.snapshotGroupMultiplier') }}</span>
      <span class="text-blue-400">{{ snapshot.group_multiplier }}×</span>
    </div>
    <p v-if="snapshot.free_fast_applied" class="text-green-400">{{ t('usage.freeFastApplied') }}</p>
    </template>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { isContentRejectionKind } from '@/utils/billingMode'
import { deepSeekMediaTools } from '@/utils/deepseekTools'
import type { UsageBillingSnapshot } from '@/types'

const props = defineProps<{ snapshot?: UsageBillingSnapshot | null }>()
const { t } = useI18n()
const isRejectionSnapshot = computed(() => isContentRejectionKind(props.snapshot?.charge_kind))
const fixedChargeAmount = computed(() => {
  const amount = props.snapshot?.fixed_charge
  return typeof amount === 'number' && Number.isFinite(amount) && amount >= 0
    ? amount.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 8 })
    : null
})
const fixedChargeText = computed(() => fixedChargeAmount.value !== null
  ? t('upstreamRejection.customerPrice', { price: fixedChargeAmount.value })
  : t('usage.snapshotNotRecorded'))

// A receipt of a standalone document or speech tool shows that tool; other
// receipts keep the Search / Fetch rows they always had.
const deepSeekToolRows = computed(() => {
  const u = props.snapshot?.deepseek_tools
  if (!u) return []
  const media = deepSeekMediaTools
    .filter(item => typeof u[item.units] === 'number' || typeof u[item.price] === 'number')
    .map(item => {
      const units = u[item.units] ?? 0
      return { tool: item.tool, units, unitLabel: `deepseekTools.${item.tool}Unit`, amount: units * (u[item.price] ?? 0) / item.per }
    })
  const calls = (['search', 'fetch'] as const).map(tool => ({
    tool, units: u[`${tool}_count`], unitLabel: '', amount: u[`${tool}_count`] * u[`${tool}_price_per_call`],
  }))
  return media.length ? [...calls.filter(row => row.units > 0), ...media] : calls
})

const tierRows = computed(() => {
  const s = props.snapshot
  if (!s) return []
  return [
    { label: t('usage.requestedTier'), value: s.requested_tier == null ? t('usage.snapshotNotRecorded') : s.requested_tier || t('usage.snapshotUnspecified') },
    { label: t('usage.forwardedTier'), value: s.forwarded_tier || 'default' },
    { label: t('usage.observedTier'), value: s.observed_tier || t('usage.snapshotNotRecorded') },
    { label: t('usage.billedTier'), value: s.billed_tier || 'default' },
  ]
})
</script>
