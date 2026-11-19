<script setup lang="ts">
import { useId } from 'vue'
import { useI18n } from 'vue-i18n'
import PlatformIcon from '@/components/common/PlatformIcon.vue'

import type { RequestSpeedTier as Tier } from '@/types'
const props = defineProps<{
  groups: Array<{ id: number; name: string }>
  modelValue: Record<string, Tier[]>
  error?: string
}>()
const emit = defineEmits<{ 'update:modelValue': [value: Record<string, Tier[]>] }>()
const { t } = useI18n()
const instanceID = useId()
const choices = ['fast', 'ultrafast', 'unassigned'] as const
function change(groupID: number, tier: Tier | 'unassigned') {
  const next = { ...props.modelValue }
  if (tier === 'unassigned') delete next[String(groupID)]
  else next[String(groupID)] = [tier]
  emit('update:modelValue', next)
}
</script>

<template>
  <section v-if="groups.length" class="mt-4 rounded-xl border border-gray-200 bg-gray-50/70 p-4 dark:border-dark-600 dark:bg-dark-800/50" data-testid="account-request-speed-fields">
    <h4 class="text-sm font-medium text-gray-700 dark:text-gray-200">{{ t('requestSpeed.accountTitle') }}</h4>
    <p class="mt-1 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t('requestSpeed.accountHint') }}</p>
    <div v-for="group in groups" :key="group.id" class="mt-4 border-t border-gray-200 pt-4 dark:border-dark-600">
      <div class="mb-3 flex items-center gap-2 text-sm font-medium text-gray-700 dark:text-gray-200"><PlatformIcon platform="openai" />{{ group.name }}</div>
      <fieldset>
        <legend class="input-label">{{ t('requestSpeed.accountTier') }}</legend>
        <div class="flex flex-wrap gap-3">
          <label v-for="tier in choices" :key="tier" class="inline-flex min-h-11 cursor-pointer items-center gap-2 rounded-lg border border-gray-200 bg-white px-3 text-sm dark:border-dark-500 dark:bg-dark-700">
            <input type="radio" :name="`${instanceID}-account-group-speed-${group.id}`" :checked="tier === 'unassigned' ? !modelValue[String(group.id)]?.length : modelValue[String(group.id)]?.length === 1 && modelValue[String(group.id)][0] === tier" class="h-4 w-4 border-gray-300 text-primary-600 focus:ring-primary-500" :data-testid="`account-group-speed-${group.id}-${tier}`" @change="change(group.id, tier)" />
            {{ tier === 'unassigned' ? t('requestSpeed.accountUnassigned') : tier === 'fast' ? 'Fast' : 'Ultrafast' }}
          </label>
        </div>
      </fieldset>
    </div>
  </section>
  <p v-if="error" role="alert" class="mt-3 rounded-lg bg-red-50 p-3 text-xs leading-relaxed text-red-700 dark:bg-red-900/20 dark:text-red-300" data-testid="account-request-speed-error">{{ error }}</p>
</template>
