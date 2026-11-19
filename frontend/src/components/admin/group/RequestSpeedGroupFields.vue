<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Toggle from '@/components/common/Toggle.vue'

defineProps<{
  enabled: boolean
  defaultTier: 'fast' | 'ultrafast'
  prefix: string
  error?: string
}>()
const emit = defineEmits<{
  'update:enabled': [value: boolean]
  'update:defaultTier': [value: 'fast' | 'ultrafast']
}>()
const { t } = useI18n()
</script>

<template>
  <div class="mb-4 rounded-xl border border-gray-200 bg-gray-50/70 p-4 dark:border-dark-600 dark:bg-dark-800/50">
    <div class="flex items-center justify-between gap-4">
      <div>
        <label :id="`${prefix}-request-speed-label`" class="text-sm font-medium text-gray-700 dark:text-gray-200">
          {{ t('requestSpeed.selectionTitle') }}
        </label>
        <p class="mt-1 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('requestSpeed.selectionHint') }}</p>
      </div>
      <Toggle :model-value="enabled" :aria-labelledby="`${prefix}-request-speed-label`" :data-testid="`${prefix}-request-speed-enabled`" @update:model-value="emit('update:enabled', $event)" />
    </div>
    <div v-if="enabled" class="mt-4 border-t border-gray-200 pt-4 dark:border-dark-600">
      <p class="mb-3 text-sm font-medium text-gray-800 dark:text-gray-100">{{ t('requestSpeed.modeByKey') }}</p>
      <label :for="`${prefix}-request-speed-default`" class="input-label">{{ t('requestSpeed.defaultTier') }}</label>
      <select :id="`${prefix}-request-speed-default`" :value="defaultTier" class="input" :data-testid="`${prefix}-request-speed-default`" @change="emit('update:defaultTier', ($event.target as HTMLSelectElement).value as 'fast' | 'ultrafast')">
        <option value="ultrafast">Ultrafast</option>
        <option value="fast">Fast</option>
      </select>
      <p class="mt-2 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('requestSpeed.defaultHint') }}</p>
      <p class="mt-2 text-xs leading-relaxed text-gray-600 dark:text-gray-300">{{ t('requestSpeed.policyManaged') }}</p>
    </div>
    <p v-if="error" :id="`${prefix}-request-speed-error`" role="alert" class="mt-3 rounded-lg bg-red-50 p-3 text-xs leading-relaxed text-red-700 dark:bg-red-900/20 dark:text-red-300" :data-testid="`${prefix}-request-speed-error`">{{ error }}</p>
  </div>
</template>
