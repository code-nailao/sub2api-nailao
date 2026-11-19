<template>
  <section class="space-y-3 rounded-lg border border-gray-200 p-4 dark:border-dark-400">
    <h4 class="text-sm font-medium">{{ t('deepseekTools.supplierTitle') }}</h4>
    <label class="flex items-center gap-2"><input v-model="form.deepseek_tools_enabled" type="checkbox" />{{ t('deepseekTools.supplierEnabled') }}</label>
    <div v-if="form.deepseek_tools_enabled" class="space-y-3">
      <p class="text-xs text-gray-500">{{ t('deepseekTools.reuseKey') }}</p>
      <label class="input-label">{{ t('deepseekTools.urlOverride') }}<input v-model="form.deepseek_tools_base_url" type="url" class="input mt-1" :placeholder="baseUrl" /></label>
      <p class="break-all text-xs text-gray-500">{{ t('deepseekTools.urlPreview') }}: {{ preview || '—' }}</p>
      <p class="text-xs text-gray-500">{{ t('deepseekTools.costHint') }}</p>
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
        <label class="input-label">{{ t('deepseekTools.searchCost') }}<input v-model.number="costs.deepseek_search_cost_per_call.value" class="input mt-1" type="number" min="0" step="0.001" /></label>
        <label class="input-label">{{ t('deepseekTools.fetchCost') }}<input v-model.number="costs.deepseek_fetch_cost_per_call.value" class="input mt-1" type="number" min="0" step="0.001" /></label>
      </div>
      <div class="grid grid-cols-1 gap-3 sm:grid-cols-3">
        <label v-for="item in deepSeekMediaTools" :key="item.tool" class="input-label">{{ t(`deepseekTools.${item.tool}Cost`) }}<input v-model.number="costs[item.cost].value" class="input mt-1" type="number" min="0" step="0.01" /></label>
      </div>
      <label class="flex items-start gap-2 text-xs"><input v-model="form.deepseek_native_tools_enabled" type="checkbox" class="mt-0.5" />{{ t('deepseekTools.nativeHint') }}</label>
    </div>
  </section>
</template>
<script setup lang="ts">
import { computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { deepSeekMediaTools, deepSeekToolsURL, type DeepSeekToolAccountForm } from '@/utils/deepseekTools'
const { t } = useI18n()
const props = defineProps<{ baseUrl: string }>()
const form = defineModel<DeepSeekToolAccountForm>({ required: true })
const preview = computed(() => deepSeekToolsURL(form.value.deepseek_tools_base_url || props.baseUrl))
// Supplier costs are optional (profit statistics only): an emptied input is null, not "".
type CostKey = 'deepseek_search_cost_per_call' | 'deepseek_fetch_cost_per_call' | typeof deepSeekMediaTools[number]['cost']
const cost = (key: CostKey) => computed({
  get: () => form.value[key],
  set: v => { form.value[key] = typeof v === 'number' && Number.isFinite(v) ? v : null }
})
const costs = Object.fromEntries((['deepseek_search_cost_per_call', 'deepseek_fetch_cost_per_call', ...deepSeekMediaTools.map(item => item.cost)] as CostKey[]).map(key => [key, cost(key)])) as Record<CostKey, ReturnType<typeof cost>>
</script>
