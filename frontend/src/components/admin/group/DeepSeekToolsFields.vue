<template>
  <section class="mt-4 space-y-3 border-t border-gray-200 pt-4 dark:border-dark-400">
    <h4 class="text-sm font-medium">{{ t('deepseekTools.title') }}</h4>
    <label class="flex items-center gap-2"><input v-model="config.enabled" type="checkbox" :disabled="!eligible" />{{ t('deepseekTools.enable') }}</label>
    <div v-if="config.enabled" class="space-y-3">
      <p class="text-xs text-gray-500">{{ t('deepseekTools.priceHint') }}</p>
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <label class="input-label">{{ t('deepseekTools.searchPrice') }}<input v-model.number="searchPerCall" class="input mt-1" type="number" min="0" step="0.001" required /></label>
        <label class="input-label">{{ t('deepseekTools.fetchPrice') }}<input v-model.number="fetchPerCall" class="input mt-1" type="number" min="0" step="0.001" required /></label>
      </div>
      <label class="input-label">{{ t('deepseekTools.account') }}
        <select v-model="config.account_id" class="input mt-1">
          <option :value="null">{{ t('deepseekTools.followAccount') }}</option>
          <option v-for="account in accounts" :key="account.id" :value="account.id">{{ account.id }} · {{ account.name }}</option>
          <option v-if="config.account_id && !accounts.some(a => a.id === config.account_id)" :value="config.account_id">#{{ config.account_id }}</option>
        </select>
      </label>
      <p v-if="loadError" class="text-xs text-red-500">{{ t('deepseekTools.accountsError') }}</p>
      <p class="text-xs text-gray-500">{{ t('deepseekTools.accountHint') }}</p>
      <label class="flex items-center gap-2"><input v-model="config.auto_inject" type="checkbox" />{{ t('deepseekTools.autoDefault') }}</label>
    </div>
  </section>
</template>
<script setup lang="ts">
import { computed, onMounted, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { list } from '@/api/admin/accounts'
import type { DeepSeekToolsConfig } from '@/types'
const { t } = useI18n()
const props = defineProps<{ eligible: boolean }>()
const config = defineModel<DeepSeekToolsConfig>({ required: true })
const searchPrice = defineModel<number | null>('searchPrice', { required: true })
const searchPerCall = computed({ get: () => searchPrice.value == null ? null : searchPrice.value / 1000, set: v => { searchPrice.value = typeof v === 'number' ? v * 1000 : null } })
const fetchPerCall = computed({ get: () => config.value.fetch_price_per_1k == null ? null : config.value.fetch_price_per_1k / 1000, set: v => { config.value = { ...config.value, fetch_price_per_1k: typeof v === 'number' ? v * 1000 : null } } })
watch(() => config.value.enabled, enabled => {
  if (!enabled) return
  if (searchPrice.value == null) searchPrice.value = 6
  if (config.value.fetch_price_per_1k == null) config.value.fetch_price_per_1k = 6
})
watch(() => props.eligible, eligible => { if (!eligible) config.value.enabled = false })
const accounts = ref<Array<{ id: number; name: string }>>([])
const loadError = ref(false)
onMounted(async () => {
  try {
    let page = 1
    for (;;) {
      const result = await list(page, 100, { platform: 'deepseek', type: 'apikey' })
      accounts.value.push(...result.items)
      if (accounts.value.length >= result.total || result.items.length === 0) break
      page++
    }
  } catch { loadError.value = true }
})
</script>
