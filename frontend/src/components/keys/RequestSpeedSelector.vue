<template>
  <fieldset class="rounded-xl border border-gray-200 bg-gray-50/60 p-4 dark:border-dark-600 dark:bg-dark-800/60" data-testid="request-speed-selector">
    <legend class="px-1 text-sm font-semibold text-gray-800 dark:text-gray-100">{{ t('requestSpeed.title') }}</legend>
    <p class="mb-3 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('requestSpeed.keyHint', { tier: label(defaultTier) }) }}</p>
    <div class="grid grid-cols-2 gap-3">
      <label v-for="tier in tiers" :key="tier" class="relative" :class="availableTiers.includes(tier) ? 'cursor-pointer' : 'cursor-not-allowed'">
        <input class="peer sr-only" type="radio" name="request-speed" :value="tier" :checked="modelValue === tier" :disabled="!availableTiers.includes(tier)" @change="emit('update:modelValue', tier)" />
        <span class="flex min-h-[76px] items-center gap-2.5 rounded-lg border border-gray-200 bg-white p-3 transition-colors peer-checked:border-primary-500 peer-checked:ring-1 peer-checked:ring-primary-500 peer-focus-visible:outline peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-primary-500 peer-disabled:bg-gray-100/80 peer-disabled:text-gray-400 dark:border-dark-600 dark:bg-dark-800 dark:peer-disabled:bg-dark-900 dark:peer-disabled:text-dark-500">
          <Icon :name="modelValue === tier ? 'checkCircle' : 'bolt'" size="md" :class="modelValue === tier ? 'text-primary-600 dark:text-primary-400' : ''" aria-hidden="true" />
          <span class="min-w-0"><span class="block whitespace-nowrap text-sm font-semibold">{{ label(tier) }}</span><span class="mt-1 block text-xs text-gray-500 dark:text-gray-400">{{ !availableTiers.includes(tier) ? t('requestSpeed.unavailable') : tier === defaultTier ? t('requestSpeed.groupDefault') : t('requestSpeed.available') }}</span></span>
        </span>
      </label>
    </div>
    <p v-if="availableTiers.length < tiers.length" class="mt-3 text-xs leading-5 text-amber-700 dark:text-amber-300">{{ t('requestSpeed.unavailableHint') }}</p>
    <p class="mt-3 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('requestSpeed.billingHint') }}</p>
  </fieldset>
</template>

<script setup lang="ts">
import { useI18n } from 'vue-i18n'
import Icon from '@/components/icons/Icon.vue'

import type { RequestSpeedTier } from '@/types'
defineProps<{ modelValue: RequestSpeedTier | null; defaultTier: RequestSpeedTier; availableTiers: RequestSpeedTier[] }>()
const emit = defineEmits<{ (event: 'update:modelValue', tier: RequestSpeedTier): void }>()
const { t } = useI18n()
const tiers: RequestSpeedTier[] = ['fast', 'ultrafast']
const label = (tier: RequestSpeedTier) => tier === 'fast' ? 'Fast' : 'Ultrafast'
</script>
