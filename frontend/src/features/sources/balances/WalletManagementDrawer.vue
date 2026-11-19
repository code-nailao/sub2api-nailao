<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Toggle from '@/components/common/Toggle.vue'
import BalanceDrawer from './BalanceDrawer.vue'
import { balanceAPI, walletManagementAPI } from './api'
import { balanceErrorKey, decimalAmount } from './presentation'
import { formatDateTime } from '@/utils/format'
import type { AccountBalance, BalanceAccount, ManagedWallet, WalletSettingsInput, WalletSettingsView, WalletValue } from './types'

const props = defineProps<{ account: BalanceAccount | null; summary?: AccountBalance }>()
const emit = defineEmits<{ close: []; changed: [summary: AccountBalance] }>()
const { t } = useI18n()
const label = (key: string, args: Record<string, unknown> = {}) => t(`sourcesBalance.management.${key}`, args)
const view = ref<WalletSettingsView>()
const draft = ref<ManagedWallet>()
const draftBalance = ref<WalletValue>()
const originalWallet = ref<ManagedWallet>()
const originalMembers = ref<number[]>([])
const members = ref<number[]>([])
const active = ref<'alerts' | 'members' | 'query'>('alerts')
const loading = ref(false)
const saving = ref(false)
const verifying = ref(false)
const choosing = ref(false)
const error = ref('')
const notice = ref('')
const search = ref('')
const email = ref('')
const confirmed = ref(false)
const explicitConfirmation = ref(false)
const existing = ref('')
const preview = ref<{ subject: string; html: string }>()
let epoch = 0
let operation = 0
let selection = 0
let controller: AbortController | undefined

function project(wallet: ManagedWallet): ManagedWallet {
  return { id: wallet.id, revision: wallet.revision, name: wallet.name, provider: wallet.provider,
    representative_id: wallet.representative_id, source_id: wallet.source_id || '',
    alert: { ...wallet.alert, recipients: [...(wallet.alert.recipients ?? [])] } }
}
const walletBalance = computed(() => draftBalance.value)
const amount = computed(() => walletBalance.value?.remaining !== undefined ? decimalAmount(walletBalance.value.remaining) : '—')
const unit = computed(() => walletBalance.value?.unit || draft.value?.alert.unit || '')
const balanceStatus = computed(() => walletBalance.value?.querying ? t('sourcesBalance.refreshBusy')
  : walletBalance.value?.error ? t(walletBalance.value.error === 'upstream_timeout' ? 'sourcesBalance.queryTimeout' : 'sourcesBalance.failed')
    : walletBalance.value?.status === 'low' ? t('sourcesBalance.low')
      : walletBalance.value?.status === 'ready' ? label('updated') : t('sourcesBalance.pending'))
const selected = computed(() => view.value?.candidates.filter(c => members.value.includes(c.id)) ?? [])
const candidates = computed(() => (view.value?.candidates ?? []).filter(c => `${c.name} ${c.site} ${c.id}`.toLowerCase().includes(search.value.toLowerCase())))
const topologyUnchanged = computed(() => Boolean(draft.value?.id && originalWallet.value &&
  draft.value.id === originalWallet.value.id && draft.value.provider === originalWallet.value.provider &&
  draft.value.source_id === originalWallet.value.source_id && draft.value.representative_id === originalWallet.value.representative_id &&
  members.value.length === originalMembers.value.length && members.value.every(id => originalMembers.value.includes(id))))
const verifiedBalance = computed(() => Boolean(walletBalance.value && ['ready', 'low'].includes(walletBalance.value.status) &&
  !walletBalance.value.stale && walletBalance.value.checked_at && walletBalance.value.unit && walletBalance.value.remaining !== undefined))
const queryAccount = computed<BalanceAccount | null>(() => {
  if (!props.account) return null
  const candidate = view.value?.candidates.find(c => c.id === draft.value?.representative_id)
  return draft.value?.provider === 'sub' && candidate
    ? { id: candidate.id, name: candidate.name, type: 'apikey', platform: candidate.platform || props.account.platform } : props.account
})
const unitMismatch = computed(() => Boolean(draft.value?.alert.unit && walletBalance.value?.unit && draft.value.alert.unit !== walletBalance.value.unit))
const canSave = computed(() => {
  const wallet = draft.value
  if (!view.value?.bridge_ready || !wallet?.name.trim() || loading.value || saving.value || verifying.value || choosing.value || !members.value.length) return false
  // Turning off an existing rule must not depend on the supplier being online
  // or implicitly confirm a changed channel identity.
  if (topologyUnchanged.value && !wallet.alert.enabled && !explicitConfirmation.value) return true
  if (members.value.length > 1 && !confirmed.value || selected.value.some(candidate => candidate.needs_confirmation) && !explicitConfirmation.value) return false
  const oldRule = originalWallet.value?.alert
  const changedThreshold = wallet.alert.enabled && (!oldRule?.enabled || wallet.alert.threshold !== oldRule.threshold || wallet.alert.unit !== oldRule.unit)
  if ((!wallet.id || !topologyUnchanged.value || explicitConfirmation.value || changedThreshold) && !verifiedBalance.value) return false
  return !wallet.alert.enabled || !unitMismatch.value && wallet.alert.threshold.trim() !== ''
})

function applyDraft(result: WalletSettingsView) {
  draft.value = project(result.wallet)
  originalWallet.value = project(result.wallet)
  draftBalance.value = { ...result.balance }
  originalMembers.value = result.members.map(candidate => candidate.id)
  members.value = [...originalMembers.value]
  if (!draft.value.alert.unit) draft.value.alert.unit = result.balance.unit || ''
  confirmed.value = result.members.every(candidate => !candidate.needs_confirmation)
  explicitConfirmation.value = false
  existing.value = result.wallet.id
}

async function load(preserve = false) {
  const account = props.account
  if (!account) return
  const turn = ++epoch
  ++selection; choosing.value = false
  controller?.abort(); controller = new AbortController()
  verifying.value = false
  loading.value = true; error.value = ''
  try {
    const result = await walletManagementAPI.get(account.id, controller.signal)
    if (turn !== epoch || props.account?.id !== account.id) return
    view.value = result
    if (!preserve || !draft.value) {
      applyDraft(result)
    } else if (draft.value.id === result.wallet.id && draft.value.representative_id === result.wallet.representative_id) {
      draftBalance.value = { ...result.balance }
      if (!draft.value.alert.unit) draft.value.alert.unit = result.balance.unit || ''
    }
    if (!result.bridge_ready) notice.value = label('syncPending')
  } catch (cause) {
    if (turn === epoch) { error.value = t(balanceErrorKey(cause)); active.value = 'query' }
  } finally { if (turn === epoch) loading.value = false }
}
watch(() => props.account?.id, () => {
  ++operation
  view.value = undefined; draft.value = undefined; draftBalance.value = undefined; originalWallet.value = undefined; originalMembers.value = []; members.value = []; preview.value = undefined
  active.value = 'alerts'; search.value = ''; email.value = ''; notice.value = ''; saving.value = false; verifying.value = false; choosing.value = false; explicitConfirmation.value = false
  if (props.account) void load()
  else { ++epoch; controller?.abort() }
}, { immediate: true })
onBeforeUnmount(() => { ++epoch; controller?.abort() })

function toggleMember(id: number, checked: boolean) {
  if (id === props.account?.id || id === draft.value?.representative_id) return
  members.value = checked ? [...members.value, id] : members.value.filter(value => value !== id)
  confirmed.value = false; explicitConfirmation.value = false; notice.value = ''; preview.value = undefined
}
function addEmail() {
  if (!draft.value) return
  const value = email.value.trim()
  if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(value) || value.length > 254 || draft.value.alert.recipients.length >= 10) { error.value = label('invalidEmail'); return }
  if (!draft.value.alert.recipients.some(v => v.toLowerCase() === value.toLowerCase())) draft.value.alert.recipients.push(value)
  email.value = ''; error.value = ''; preview.value = undefined
}
async function chooseWallet() {
  if (!props.account || !view.value) return
  const choice = ++selection
  if (!existing.value) {
    applyDraft(view.value); choosing.value = false
    return
  }
  const candidate = view.value.candidates.find(c => c.wallet_id === existing.value)
  if (!candidate) return
  const turn = epoch
  choosing.value = true
  try {
    const result = await walletManagementAPI.get(candidate.id, controller?.signal)
    if (turn !== epoch || choice !== selection) return
    applyDraft(result)
    view.value.candidates = result.candidates
    members.value = [...new Set([...members.value, props.account.id])]
    confirmed.value = false; notice.value = ''; preview.value = undefined
  } catch (cause) { if (turn === epoch) error.value = t(balanceErrorKey(cause)) }
  finally { if (turn === epoch && choice === selection) choosing.value = false }
}
async function publish() {
  if (!props.account) return
  const [summary] = await balanceAPI.summaries([props.account.id], controller?.signal)
  if (summary) emit('changed', summary)
}
async function save() {
  const account = props.account, wallet = draft.value
  if (!account || !wallet || !canSave.value) return
  const turn = epoch
  const action = ++operation
  saving.value = true; error.value = ''; notice.value = ''
  const input: WalletSettingsInput = { ...project(wallet), members: [...members.value], confirm_shared: explicitConfirmation.value && confirmed.value }
  try {
    await walletManagementAPI.save(account.id, input, controller?.signal)
    if (turn !== epoch) return
    await publish()
    if (turn !== epoch) return
    await load(); if (props.account?.id === account.id) notice.value = label('saved')
  } catch (cause) { if (turn === epoch) error.value = t(balanceErrorKey(cause)) }
  finally { if (action === operation) saving.value = false }
}
async function unlink() {
  const account = props.account, wallet = view.value?.wallet
  if (!account || !wallet?.id || saving.value) return
  const turn = epoch
  const action = ++operation
  saving.value = true; error.value = ''
  try {
    await walletManagementAPI.unlink(account.id, wallet, controller?.signal)
    if (turn !== epoch) return
    await publish(); if (turn !== epoch) return
    await load(); if (props.account?.id === account.id) notice.value = label('unlinked')
  } catch (cause) { if (turn === epoch) error.value = t(balanceErrorKey(cause)) }
  finally { if (action === operation) saving.value = false }
}
async function verify() {
  const wallet = draft.value
  if (!wallet || verifying.value) return
  const turn = epoch
  const representativeID = wallet.representative_id
  verifying.value = true; error.value = ''; notice.value = ''
  try {
    const value = await walletManagementAPI.verify(representativeID, controller?.signal)
    if (turn !== epoch || draft.value !== wallet || wallet.representative_id !== representativeID) return
    draftBalance.value = { ...value }
    if (value.status !== 'ready') { error.value = t(balanceErrorKey(value)); return }
    if (!wallet.alert.unit) wallet.alert.unit = value.unit || ''
    const candidate = view.value?.candidates.find(c => c.id === wallet.representative_id)
    if (candidate) candidate.verified = true
    if (members.value.length === 1) { confirmed.value = true; explicitConfirmation.value = true }
    notice.value = label('channelVerified', { amount: decimalAmount(value.remaining || '0'), unit: value.unit })
  } catch (cause) { if (turn === epoch) error.value = t(balanceErrorKey(cause)) }
  finally { if (turn === epoch) verifying.value = false }
}
function changeQueryChannel() {
  draftBalance.value = { status: 'pending', stale: false }
  confirmed.value = false; explicitConfirmation.value = false; notice.value = ''; preview.value = undefined
}
async function previewMail() {
  if (!props.account || !draft.value) return
  const turn = epoch
  try {
    const result = await walletManagementAPI.preview(props.account.id, { name: draft.value.name,
      alert: draft.value.alert, amount: draft.value.alert.threshold || '0', names: selected.value.map(c => c.name) }, controller?.signal)
    if (turn === epoch) preview.value = result
  } catch (cause) { if (turn === epoch) error.value = t(balanceErrorKey(cause)) }
}
async function queryChanged(summary: AccountBalance) {
  emit('changed', summary)
  const wallet = draft.value
  if (!wallet || summary.source_id || wallet.provider === 'new') { await load(); return }
  await load(true)
  if (draft.value !== wallet || summary.account_id !== wallet.representative_id ||
    summary.representative_id && summary.representative_id !== wallet.representative_id ||
    (summary.wallet_id || '') !== wallet.id) return
  draftBalance.value = { status: summary.status, stale: summary.stale, error: summary.error,
    remaining: summary.balance?.remaining, unit: summary.balance?.unit, checked_at: summary.balance?.checked_at,
    querying: summary.querying, query_url: summary.query_url, auto_refresh: summary.auto_refresh,
    next_check_at: summary.next_check_at, last_attempt_at: summary.last_attempt_at }
  if (!wallet.alert.unit) wallet.alert.unit = draftBalance.value.unit || ''
}
</script>

<template>
  <BaseDialog :show="Boolean(account)" :title="label('title')" placement="right" :show-close-button="true" :close-on-escape="true" @close="emit('close')">
    <div class="space-y-5" data-testid="wallet-management">
      <p v-if="loading" role="status" class="text-sm text-gray-500">{{ t('sourcesBalance.loading') }}</p>
      <section v-if="view" class="space-y-2 border-b border-gray-100 pb-5 dark:border-dark-700">
        <div class="flex items-center justify-between gap-3"><h3 class="text-base font-semibold text-gray-900 dark:text-white">{{ draft?.name || view.wallet.name }}</h3><span class="rounded-md px-2 py-1 text-xs" :class="walletBalance?.status === 'ready' && !walletBalance?.stale ? 'bg-emerald-50 text-emerald-700 dark:bg-emerald-900/20 dark:text-emerald-300' : 'bg-amber-50 text-amber-700 dark:bg-amber-900/20 dark:text-amber-300'">{{ balanceStatus }}</span></div>
        <p class="break-all text-xs text-gray-500">{{ walletBalance?.query_url || account?.name }}</p>
        <p class="break-all text-3xl font-semibold tabular-nums text-gray-900 dark:text-white">{{ amount }} <span class="text-sm font-normal text-gray-500">{{ unit }}</span></p>
        <div class="flex flex-wrap items-center justify-between gap-2 text-xs text-gray-500"><span>{{ walletBalance?.checked_at ? t('sourcesBalance.lastSuccess', { time: formatDateTime(walletBalance.checked_at) }) : t('sourcesBalance.never') }}</span><button type="button" class="min-h-9 text-primary-600 dark:text-primary-400" @click="active = 'members'">{{ label('sharedCount', { count: view.members.length }) }} →</button></div>
        <p v-if="walletBalance?.error" class="text-xs text-amber-700 dark:text-amber-300">{{ t(balanceErrorKey(walletBalance)) }}</p>
      </section>
      <div class="flex gap-5 border-b border-gray-200 dark:border-dark-700" role="tablist" :aria-label="label('tabs')">
        <button v-for="tab in (['alerts', 'members', 'query'] as const)" :id="`wallet-tab-${tab}`" :key="tab" type="button" role="tab" :aria-selected="active === tab" :aria-controls="`wallet-panel-${tab}`" class="min-h-11 border-b-2 px-1 text-sm" :class="active === tab ? 'border-primary-500 font-medium text-primary-600 dark:text-primary-400' : 'border-transparent text-gray-500'" @click="active = tab">{{ label(tab) }}</button>
      </div>
      <p v-if="error" role="alert" class="rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">{{ error }}</p>
      <p v-if="notice" role="status" class="text-sm text-primary-700 dark:text-primary-300">{{ notice }}</p>

      <section v-if="active === 'alerts' && draft" id="wallet-panel-alerts" role="tabpanel" aria-labelledby="wallet-tab-alerts" class="space-y-5">
        <div v-if="unitMismatch" role="alert" class="rounded-lg bg-amber-50 p-3 text-xs text-amber-800 dark:bg-amber-900/20 dark:text-amber-200"><p>{{ label('unitChanged') }}</p><button type="button" class="mt-2 min-h-10 underline" @click="draft.alert.unit = walletBalance?.unit || ''; draft.alert.threshold = ''; preview = undefined">{{ label('useCurrentUnit', { unit: walletBalance?.unit }) }}</button></div>
        <label class="flex min-h-11 items-center justify-between gap-4"><span><span class="block text-sm font-medium text-gray-900 dark:text-white">{{ label('enable') }}</span><span class="mt-1 block text-xs text-gray-500">{{ label('enableHint') }}</span></span><Toggle v-model="draft.alert.enabled" :disabled="saving || !draft.alert.enabled && !walletBalance?.unit" :aria-label="label('enable')" /></label>
        <p v-if="!walletBalance?.unit" class="text-xs text-gray-500">{{ label('queryBeforeAlerts') }} <button type="button" class="min-h-10 text-primary-600 underline" @click="active = 'query'">{{ t('sourcesBalance.queryOnce') }}</button></p>
        <fieldset :disabled="!draft.alert.enabled || saving" class="space-y-5 disabled:opacity-50">
          <label class="block text-sm font-medium text-gray-800 dark:text-gray-200">{{ label('threshold') }}<div class="mt-2 flex min-h-11 items-center overflow-hidden rounded-lg border border-gray-200 dark:border-dark-600"><span class="whitespace-nowrap border-r border-gray-200 px-3 text-xs font-normal text-gray-500 dark:border-dark-600">{{ label('balanceBelow') }}</span><input :value="draft.alert.threshold" type="number" min="0" step="any" inputmode="decimal" class="w-full min-w-0 bg-transparent px-3 py-2 text-base tabular-nums outline-offset-2" @input="draft.alert.threshold = ($event.target as HTMLInputElement).value; preview = undefined" /><span class="px-3 text-xs font-normal text-gray-500">{{ draft.alert.unit || label('unitPending') }}</span></div><span class="mt-2 block text-xs font-normal text-gray-500">{{ label('unitHint') }}</span></label>
          <div class="space-y-2"><span class="block text-sm font-medium text-gray-800 dark:text-gray-200">{{ label('recipients') }}</span><label class="flex min-h-10 items-center gap-2 text-sm"><input v-model="draft.alert.use_default_recipients" type="checkbox" class="accent-primary-600" />{{ label('defaultRecipients') }}</label><p class="break-all text-xs text-gray-500">{{ view?.default_recipients.length ? view.default_recipients.join('、') : label('noDefaultRecipients') }}</p><div class="flex flex-wrap gap-2"><span v-for="recipient in draft.alert.recipients" :key="recipient" class="inline-flex max-w-full items-center gap-2 rounded-md bg-gray-100 px-2 py-1 text-xs dark:bg-dark-700"><span class="break-all">{{ recipient }}</span><button type="button" class="min-h-8 min-w-7" :aria-label="label('removeEmail', { email: recipient })" @click="draft.alert.recipients = draft.alert.recipients.filter(v => v !== recipient)">×</button></span></div><div class="flex gap-2"><input v-model="email" type="email" class="input min-w-0 flex-1" :aria-label="label('extraEmail')" placeholder="name@example.com" @keydown.enter.prevent="addEmail" /><button type="button" class="btn btn-secondary" @click="addEmail">{{ label('add') }}</button></div></div>
          <label class="block text-sm font-medium text-gray-800 dark:text-gray-200">{{ label('frequency') }}<select v-model="draft.alert.repeat_hours" class="input mt-2"><option :value="0">{{ label('once') }}</option><option v-for="hour in [6, 12, 24]" :key="hour" :value="hour">{{ label('repeat', { hours: hour }) }}</option></select><span class="mt-2 block text-xs font-normal leading-relaxed text-gray-500">{{ label('frequencyHint') }}</span></label>
          <details class="border-t border-gray-100 pt-3 dark:border-dark-700"><summary class="min-h-9 text-sm text-gray-600 dark:text-gray-300">{{ label('more') }}</summary><label class="flex min-h-11 items-center gap-2 text-sm"><input v-model="draft.alert.notify_recovery" type="checkbox" class="accent-primary-600" />{{ label('recovery') }}</label><p class="text-xs text-gray-500">{{ label('recoveryHint') }}</p></details>
          <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-800"><div class="flex items-center justify-between gap-3 text-sm"><span>{{ label('mailPreview') }}</span><button type="button" class="min-h-9 text-primary-600 dark:text-primary-400" @click="previewMail">{{ label('preview') }} ↗</button></div><template v-if="preview"><p class="mb-2 text-xs text-gray-500">{{ label('previewHint') }}</p><iframe :title="preview.subject" :srcdoc="preview.html" sandbox="" class="h-80 w-full rounded border border-gray-200 bg-white" /></template></div>
        </fieldset>
        <details v-if="view?.deliveries.length" class="border-t border-gray-100 pt-3 dark:border-dark-700"><summary class="min-h-9 text-sm">{{ label('history') }}</summary><ul class="divide-y divide-gray-100 dark:divide-dark-700"><li v-for="item in view.deliveries" :key="item.id" class="py-3 text-xs"><div class="flex items-center justify-between gap-2"><span>{{ label(`delivery_${item.status}`) }}</span><span class="text-gray-500">{{ formatDateTime(item.updated_at) }}</span></div><p class="mt-1 break-all text-gray-500">{{ item.recipient }}</p></li></ul></details>
      </section>

      <section v-if="active === 'members' && draft" id="wallet-panel-members" role="tabpanel" aria-labelledby="wallet-tab-members" class="space-y-4">
        <p class="text-xs leading-relaxed text-gray-500">{{ label('membersHint') }}</p>
        <label class="block text-sm font-medium">{{ label('walletName') }}<input v-model="draft.name" class="input mt-2" maxlength="160" /></label>
        <label v-if="!view?.wallet.id && view?.wallets.length" class="block text-sm font-medium">{{ label('existingWallet') }}<select v-model="existing" class="input mt-2" @change="chooseWallet"><option value="">{{ label('independentWallet') }}</option><option v-for="wallet in view.wallets" :key="wallet.id" :value="wallet.id">{{ wallet.name }}</option></select></label>
        <input v-model="search" type="search" class="input" :aria-label="label('searchChannels')" :placeholder="label('searchChannels')" />
        <div class="flex items-center justify-between text-xs text-gray-500"><span>{{ label('selectChannels') }}</span><span>{{ label('selectedCount', { count: members.length }) }}</span></div>
        <div class="max-h-96 overflow-y-auto rounded-lg border border-gray-200 dark:border-dark-700"><label v-for="candidate in candidates" :key="candidate.id" class="flex min-h-16 items-center gap-3 border-b border-gray-100 p-3 last:border-0 dark:border-dark-700" :class="members.includes(candidate.id) ? 'bg-primary-50/50 dark:bg-primary-900/10' : ''"><input type="checkbox" :checked="members.includes(candidate.id)" :disabled="candidate.id === account?.id || candidate.id === draft.representative_id || Boolean(candidate.wallet_id && candidate.wallet_id !== draft.id)" class="h-4 w-4 shrink-0 accent-primary-600" @change="toggleMember(candidate.id, ($event.target as HTMLInputElement).checked)" /><span class="min-w-0 flex-1"><span class="block break-words text-xs font-medium">{{ candidate.name }}</span><span class="mt-1 block break-all text-[11px] text-gray-500">#{{ candidate.id }} · {{ candidate.site }}</span><span v-if="candidate.needs_confirmation" class="text-xs text-amber-700">{{ label('reconfirm') }}</span></span><span v-if="candidate.id === draft.representative_id && draft.provider === 'sub'" class="shrink-0 rounded bg-primary-50 px-1.5 py-1 text-[11px] text-primary-700 dark:bg-primary-900/30 dark:text-primary-300">{{ label('queryChannel') }}</span><span v-else-if="candidate.wallet_id && candidate.wallet_id !== draft.id" class="text-[11px] text-gray-500">{{ label('linkedElsewhere') }}</span></label></div>
        <label v-if="members.length > 1" class="flex min-h-11 items-start gap-2 text-xs leading-relaxed"><input v-model="confirmed" type="checkbox" class="mt-1 accent-primary-600" @change="explicitConfirmation = confirmed" />{{ label('confirmShared') }}</label>
        <button v-if="view?.wallet.id" type="button" class="min-h-10 text-sm text-gray-500 underline decoration-gray-300 underline-offset-4" :disabled="saving" @click="unlink">{{ label('unlinkCurrent') }}</button>
      </section>

      <section v-if="active === 'query'" id="wallet-panel-query" role="tabpanel" aria-labelledby="wallet-tab-query" class="space-y-5">
        <template v-if="draft?.provider === 'sub'"><label v-if="selected.length > 1" class="block text-sm font-medium">{{ label('queryChannel') }}<select v-model="draft.representative_id" class="input mt-2" :disabled="saving || verifying" @change="changeQueryChannel"><option v-for="candidate in selected" :key="candidate.id" :value="candidate.id">{{ candidate.name }}</option></select></label><div class="flex flex-wrap items-center gap-3"><button type="button" class="btn btn-secondary" :disabled="saving || verifying || choosing" data-testid="wallet-verify-channel" @click="verify">{{ verifying ? t('sourcesBalance.testingWallet') : label('verifyChannel') }}</button><span class="text-xs text-gray-500">{{ label('verifyHint') }}</span></div><p v-if="verifying" role="status" class="text-xs text-gray-500">{{ t('sourcesBalance.queryWaiting') }}</p></template>
        <BalanceDrawer :account="queryAccount" :summary="summary" :embedded="true" @close="emit('close')" @changed="queryChanged" />
      </section>
    </div>
    <template #footer><div class="flex w-full flex-wrap items-center justify-between gap-3"><span class="text-xs text-gray-500">{{ label('sharedCount', { count: members.length || 1 }) }}</span><div class="flex gap-2"><button type="button" class="btn btn-secondary" @click="emit('close')">{{ t('sourcesBalance.close') }}</button><button v-if="draft" type="button" class="btn btn-primary" :disabled="!canSave" @click="save">{{ saving ? label('saving') : label('save') }}</button></div></div></template>
  </BaseDialog>
</template>
