<template>
  <div
    v-if="visible"
    data-testid="upstream-rejection-notice"
    class="basis-full break-words rounded-lg border border-amber-200 bg-amber-50 px-3 py-2 text-xs leading-5 text-amber-900 dark:border-amber-700/50 dark:bg-amber-900/20 dark:text-amber-200"
  >
    <p class="font-medium">
      {{ t('upstreamRejection.title') }}：{{ t('upstreamRejection.customerPrice', { price: formattedPrice }) }}
    </p>
    <p>{{ t('upstreamRejection.hint', { price: formattedPrice }) }}</p>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { isValidRejectionAmount } from '@/utils/upstreamRejection'

const props = defineProps<{
  group: {
    platform: string
    subscription_type: string
    upstream_rejection_billing_enabled?: boolean
    upstream_rejection_price?: number
  }
}>()

const { t } = useI18n()
const visible = computed(() =>
  props.group.platform === 'grok' &&
  props.group.subscription_type === 'standard' &&
  props.group.upstream_rejection_billing_enabled === true &&
  isValidRejectionAmount(props.group.upstream_rejection_price, true)
)

// A fixed fee is the final group price, never multiplied by a user or group rate.
const formattedPrice = computed(() =>
  new Intl.NumberFormat('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 8 }).format(
    props.group.upstream_rejection_price ?? 0
  )
)
</script>
