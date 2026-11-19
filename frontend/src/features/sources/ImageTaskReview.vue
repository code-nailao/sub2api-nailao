<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import TotpStepUpDialog from '@/components/auth/TotpStepUpDialog.vue'
import { apiClient } from '@/api/client'
import { useStepUp, isStepUpCancelled } from '@/composables/useStepUp'

const { t } = useI18n()
interface Task {
  id: string; phase: string; user_id: number; api_key_id: number; group_id: number; account_id: number
  model?: string; reserved_amount?: number; created_at: number; upstream_reference?: string
  recovery_status?: string; recovery_deadline_at?: number; manual_action_required?: boolean
}
const items = ref<Task[]>([])
const phase = ref('unknown')
const busy = ref(false)
const saving = ref(false)
const error = ref('')
const selected = ref<Task | null>(null)
const action = ref('bind')
const reference = ref('')
const reason = ref('')
const confirmed = ref(false)
const stepUp = useStepUp()
let revision = 0
const valid = computed(() => selected.value && reason.value.trim().length >= 8 && confirmed.value && (action.value !== 'bind' || /^[A-Za-z0-9_-]{1,200}$/.test(reference.value)))
async function load(after = '') {
  const current = ++revision
  busy.value = true
  error.value = ''
  try {
    const { data } = await apiClient.get<{ items: Task[] }>('/admin/sources/image-tasks', { params: { phase: phase.value, after } })
    if (current === revision) items.value = data.items
  } catch { if (current === revision) error.value = t('sourcesImages.readFailed') }
  finally { if (current === revision) busy.value = false }
}
function review(item: Task) {
  if (item.manual_action_required === false) return
  selected.value = item
  action.value = item.phase === 'delivery_review' ? 'retry_delivery' : 'bind'
  reference.value = item.upstream_reference ?? ''
  reason.value = ''
  confirmed.value = false
}
function recoveryLabel(item: Task) {
  switch (item.recovery_status) {
    case 'disabled': return t('sourcesImages.recoveryDisabled')
    case 'excluded': return t('sourcesImages.recoveryExcluded')
    case 'recovering': return t('sourcesImages.recovering')
    case 'manual_required': return t('sourcesImages.manualRequired')
    case 'closing': return t('sourcesImages.closing')
    case 'closed_unknown': return t('sourcesImages.closedUnknown')
    default: return ''
  }
}
async function resolve() {
  if (!valid.value || saving.value || !selected.value) return
  const id = selected.value.id
  const body = { action: action.value, reference: reference.value, reason: reason.value.trim(), confirm: true }
  saving.value = true
  error.value = ''
  try {
    await stepUp.run(() => apiClient.post(`/admin/sources/image-tasks/${encodeURIComponent(id)}/resolve`, body))
    selected.value = null
    await load()
  } catch (err) { if (!isStepUpCancelled(err)) error.value = t('sourcesImages.writeFailed') }
  finally { saving.value = false }
}
onMounted(() => load())
</script>

<template>
  <AppLayout>
    <section class="space-y-4">
      <h1 class="text-xl font-semibold">{{ t('sourcesImages.title') }}</h1>
      <p class="text-sm text-gray-500 dark:text-gray-400">{{ t('sourcesImages.intro') }}</p>
      <div class="flex flex-wrap gap-3">
        <select v-model="phase" class="input w-auto" :aria-label="t('sourcesImages.title')" :disabled="busy || saving" @change="load()">
          <option value="unknown">{{ t('sourcesImages.unknown') }}</option><option value="delivery_review">{{ t('sourcesImages.delivery') }}</option>
        </select>
        <button class="btn btn-secondary" :disabled="busy || saving" @click="load()">{{ t('sourcesImages.refresh') }}</button>
        <button class="btn btn-secondary" :disabled="busy || saving || items.length < 100" @click="load(items[items.length - 1]?.id)">{{ t('sourcesImages.next') }}</button>
      </div>
      <p v-if="error" role="alert" class="text-red-600">{{ error }}</p>
      <p v-if="!busy && !items.length && !error" role="status">{{ t('sourcesImages.empty') }}</p>
      <article v-for="item in items" :key="item.id" class="card space-y-2 p-4">
        <p class="break-all font-mono text-sm">{{ item.id }}</p>
        <p class="text-sm">{{ t('sourcesImages.identity') }}: {{ item.user_id }} / {{ item.api_key_id }} / {{ item.group_id }} / {{ item.account_id }}</p>
        <p>{{ item.model }} · {{ t('sourcesImages.amount') }}: {{ item.reserved_amount?.toFixed(8) ?? '—' }}</p>
        <time>{{ new Date(item.created_at * 1000).toLocaleString() }}</time>
        <p v-if="recoveryLabel(item)" class="text-sm text-gray-600 dark:text-gray-300">{{ recoveryLabel(item) }}</p>
        <p v-if="item.recovery_deadline_at && Number.isFinite(item.recovery_deadline_at)" class="text-sm text-gray-500 dark:text-gray-400">{{ t('sourcesImages.recoveryDeadline') }}: <time>{{ new Date(item.recovery_deadline_at * 1000).toLocaleString() }}</time></p>
        <div v-if="item.manual_action_required !== false"><button class="btn btn-secondary" :disabled="saving" @click="review(item)">{{ t('sourcesImages.review') }}</button></div>
      </article>
      <BaseDialog :show="!!selected" :title="t('sourcesImages.review')" width="narrow" @close="!saving && (selected = null)">
        <form id="image-review-form" class="space-y-4" @submit.prevent="resolve">
          <p class="break-all font-mono text-sm">{{ selected?.id }}</p>
          <select v-model="action" class="input" :aria-label="t('sourcesImages.review')" :disabled="saving">
            <template v-if="selected?.phase === 'unknown'"><option value="bind">{{ t('sourcesImages.bind') }}</option><option value="release">{{ t('sourcesImages.release') }}</option></template>
            <option v-else value="retry_delivery">{{ t('sourcesImages.retry') }}</option>
          </select>
          <label v-if="action === 'bind'" class="block">{{ t('sourcesImages.reference') }}<input v-model="reference" class="input mt-1" maxlength="200" :disabled="saving" autocomplete="off" /></label>
          <label class="block">{{ t('sourcesImages.reason') }}<textarea v-model="reason" class="input mt-1" maxlength="2000" :disabled="saving" /></label>
          <label class="flex gap-2"><input v-model="confirmed" type="checkbox" :disabled="saving" /><span class="text-sm">{{ t('sourcesImages.confirm') }}</span></label>
        </form>
        <template #footer><div class="flex justify-end gap-3"><button class="btn btn-secondary" :disabled="saving" @click="selected = null">{{ t('sourcesImages.cancel') }}</button><button form="image-review-form" type="submit" class="btn btn-primary" :disabled="!valid || saving">{{ t('sourcesImages.save') }}</button></div></template>
      </BaseDialog>
      <TotpStepUpDialog :controller="stepUp" />
    </section>
  </AppLayout>
</template>
