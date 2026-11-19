<template>
  <div class="cache-hit-rate-cell whitespace-nowrap text-xs tabular-nums">
    <div
      data-testid="cache-hit-rate"
      class="flex items-center gap-1"
      :class="cacheHitRate === null ? 'text-gray-400 dark:text-gray-500' : 'text-blue-600 dark:text-blue-400'"
    >
      <Icon name="database" size="xs" class="shrink-0" aria-hidden="true" />
      <span>{{ t('usage.performanceCache') }}: {{ cacheHitRate === null ? '-' : `${cacheHitRate}%` }}</span>
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{
  usage: {
    input_tokens?: number | null
    cache_creation_tokens?: number | null
    cache_read_tokens?: number | null
  }
}>()

const { t } = useI18n()

const cacheHitRate = computed(() => {
  const tokens = [props.usage.input_tokens, props.usage.cache_creation_tokens, props.usage.cache_read_tokens]
  if (!tokens.every((value): value is number => typeof value === 'number' && Number.isFinite(value) && value >= 0)) return null
  // Same prompt-token denominator as TokenUsageTrend; missing data is not a 0% hit rate.
  const totalPromptTokens = tokens.reduce((total, value) => total + value, 0)
  if (!Number.isFinite(totalPromptTokens) || totalPromptTokens <= 0) return null
  return (tokens[2] / totalPromptTokens * 100).toFixed(1)
})
</script>
