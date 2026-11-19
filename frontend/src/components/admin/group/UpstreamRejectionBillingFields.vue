<template>
  <section class="mt-4 border-t border-gray-200 pt-4 dark:border-dark-400" :aria-labelledby="`${idPrefix}-rejection-title`">
    <h4 :id="`${idPrefix}-rejection-title`" class="text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('upstreamRejection.title') }}</h4>
    <label class="mt-3 flex min-h-11 items-center gap-2">
      <input type="checkbox" :checked="enabled" :disabled="eligible === false && !enabled" :data-testid="`${idPrefix}-rejection-enabled`" @change="onToggle" />
      <span class="text-sm text-gray-700 dark:text-gray-300">{{ t('upstreamRejection.enabled') }}</span>
    </label>
    <p v-if="eligible === false" class="mt-2 text-sm text-amber-700 dark:text-amber-400">{{ t('upstreamRejection.eligibleOnly') }}</p>
    <template v-if="enabled">
      <label :for="`${idPrefix}-rejection-price`" class="input-label">{{ t('upstreamRejection.price') }}</label>
      <input :id="`${idPrefix}-rejection-price`" type="number" min="0.00000001" step="0.00000001" required :value="price" class="input" :data-testid="`${idPrefix}-rejection-price`" :aria-invalid="!valid" :aria-describedby="valid ? `${idPrefix}-rejection-hint` : `${idPrefix}-rejection-hint ${idPrefix}-rejection-error`" @input="emit('update:price', ($event.target as HTMLInputElement).value === '' ? '' : Number(($event.target as HTMLInputElement).value))" />
    </template>
    <p :id="`${idPrefix}-rejection-hint`" class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('upstreamRejection.hint', { price: priceText }) }}</p>
    <p class="mt-2 text-xs text-gray-500 dark:text-gray-400">{{ t('upstreamRejection.groupCompatibility') }}</p>
    <p v-if="!valid" :id="`${idPrefix}-rejection-error`" role="alert" class="mt-2 text-sm text-red-600 dark:text-red-400">{{ t('upstreamRejection.invalidPrice') }}</p>
  </section>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { isValidRejectionAmount } from '@/utils/upstreamRejection'
const props = withDefaults(defineProps<{ enabled: boolean; price: number | string; idPrefix: string; eligible?: boolean }>(), { eligible: true })
const emit = defineEmits<{ 'update:enabled': [value: boolean]; 'update:price': [value: number | string] }>()
const { t } = useI18n()
function onToggle(event: Event) {
  const enabled = (event.target as HTMLInputElement).checked
  // Turning off the fee must remain possible after clearing/invalidating its input.
  if (!enabled && !isValidRejectionAmount(props.price, false)) emit('update:price', 0.1)
  emit('update:enabled', enabled)
}
const priceText = computed(() => {
  const amount = props.price
  return isValidRejectionAmount(amount, false) ? amount.toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 8 }) : '—'
})
const valid = computed(() => isValidRejectionAmount(props.price, props.enabled))
</script>
