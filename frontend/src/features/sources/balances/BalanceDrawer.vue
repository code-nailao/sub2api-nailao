<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import BalanceSurface from './BalanceSurface.vue'
import AccountBalanceCell from './AccountBalanceCell.vue'
import { balanceAPI } from './api'
import { balanceErrorKey, decimalAmount, identityChanged, normalizeWalletURL, sourceVerified } from './presentation'
import { formatDateTime } from '@/utils/format'
import type { AccountBalance, BalanceAccount, BalanceSource, SourceInput, WalletConfiguration, WalletProbe } from './types'

const props = defineProps<{ account: BalanceAccount | null; summary?: AccountBalance; embedded?: boolean }>()
const emit = defineEmits<{ close: []; changed: [summary: AccountBalance] }>()
const { t } = useI18n()
const sources = ref<BalanceSource[]>([])
const selectedID = ref('')
const editor = ref(false)
const mode = ref<'automatic' | 'sub' | 'new'>('automatic')
const walletConfig = ref<WalletConfiguration>()
const walletURL = ref('')
const probe = ref<WalletProbe>()
const normalizedURL = computed(() => normalizeWalletURL(walletURL.value))
const manualBound = computed(() => Boolean(current.value?.source_id || current.value?.status === 'rebind_required' && current.value.provider !== 'sub'))
const canSaveWallet = computed(() => !manualBound.value && Boolean(walletConfig.value && probe.value?.verification_token && probe.value.url === normalizedURL.value && probe.value.wallet.status === 'ready'))
watch(walletURL, () => { probe.value = undefined; error.value = ''; notice.value = '' })
watch(mode, () => { probe.value = undefined; error.value = ''; notice.value = ''; editor.value = false; form.value.personal_token = '' })
const editingID = ref<string | null>(null)
const loading = ref(false)
const busy = ref(false)
const querying = ref(false)
const error = ref('')
const notice = ref('')
let epoch = 0
let abort: AbortController | undefined
const emptyForm = (): SourceInput => ({ name: '', origin: '', account_id: 0, personal_token: '', threshold: '', threshold_unit: '', revision: 0 })
const form = ref(emptyForm())
const upstreamID = ref('')
const selected = computed(() => sources.value.find(source => source.id === selectedID.value))
const bindingRevision = ref(0)
const current = ref<AccountBalance>()
const supported = computed(() => props.account?.type === 'apikey' && props.account.platform !== 'antigravity')
const shared = computed(() => current.value?.source_id === selectedID.value ? current.value.shared_count : 0)
const verified = computed(() => sourceVerified(selected.value))
const editingExisting = computed(() => sources.value.find(source => source.id === editingID.value))

function storeSource(source: BalanceSource) {
  sources.value = [...sources.value.filter(item => item.id !== source.id), source]
  selectedID.value = source.id
}
function resetEditor(source?: BalanceSource) {
  error.value = ''; notice.value = ''; editingID.value = source?.id ?? null
  form.value = source ? { name: source.name, origin: source.origin, account_id: source.account_id,
    personal_token: '', threshold: source.threshold, threshold_unit: source.threshold_unit, revision: source.revision } : emptyForm()
  upstreamID.value = source?.account_id ? String(source.account_id) : ''
  editor.value = true
}
async function load() {
  const version = ++epoch
  abort?.abort(); abort = new AbortController()
  const account = props.account
  if (!account) return
  loading.value = true; busy.value = false; querying.value = false; error.value = ''; notice.value = ''; editor.value = false; probe.value = undefined
  form.value.personal_token = ''
  try {
    const [list, balances, config] = await Promise.all([
      balanceAPI.sources(abort.signal), balanceAPI.summaries([account.id], abort.signal),
      balanceAPI.walletConfiguration(account.id, abort.signal)
    ])
    if (version !== epoch || props.account?.id !== account.id) return
    sources.value = list
    current.value = balances.find(item => item.account_id === account.id)
    bindingRevision.value = current.value?.binding_revision ?? 0
    walletConfig.value = config; walletURL.value = config.url
    mode.value = manualBound.value ? 'new' : config.url ? 'sub' : 'automatic'
    selectedID.value = current.value?.status === 'rebind_required' ? '' : current.value?.source_id ?? ''
    if (current.value?.status === 'unbound' && !current.value.source_id) {
      const automatic = await balanceAPI.automatic(account.id, false, abort.signal)
      if (version !== epoch || props.account?.id !== account.id) return
      current.value = { ...automatic, binding_revision: bindingRevision.value }
    }
  } catch (cause) {
    if (version === epoch) error.value = t(balanceErrorKey(cause))
  } finally {
    if (version === epoch) loading.value = false
  }
}
watch(() => props.account?.id, () => {
  walletConfig.value = undefined; probe.value = undefined; walletURL.value = ''
  current.value = props.summary
  bindingRevision.value = props.summary?.binding_revision ?? 0
  if (props.account) void load()
  else { ++epoch; abort?.abort(); querying.value = false; form.value = emptyForm(); sources.value = [] }
}, { immediate: true })
onBeforeUnmount(() => { ++epoch; abort?.abort(); form.value.personal_token = '' })
const sessionChanged = (event: StorageEvent) => {
  if (identityChanged(event)) {
    ++epoch; abort?.abort(); form.value.personal_token = ''; sources.value = []; current.value = undefined; walletConfig.value = undefined; probe.value = undefined
    emit('close')
  }
}
window.addEventListener('storage', sessionChanged)
onBeforeUnmount(() => window.removeEventListener('storage', sessionChanged))

async function action(work: (valid: () => boolean) => Promise<void>) {
  if (busy.value) return
  const version = epoch
  busy.value = true; error.value = ''; notice.value = ''
  try { await work(() => version === epoch && Boolean(props.account)) } catch (cause) { if (version === epoch) error.value = t(balanceErrorKey(cause)) }
  finally { if (version === epoch) { busy.value = false; querying.value = false } }
}
function publish(summary: AccountBalance) {
  current.value = summary; bindingRevision.value = summary.binding_revision; emit('changed', summary)
}
async function refreshAutomatic() {
  const account = props.account
  if (!account) return
  await action(async valid => {
    querying.value = true
    const result = await balanceAPI.automatic(account.id, true, abort?.signal)
    if (valid()) {
      // Identity failures intentionally omit the wallet from the raw reader.
      // Reload canonical ownership so a Sub rebind never becomes a New binding.
      const latest = result.status === 'rebind_required' ? (await balanceAPI.summaries([account.id], abort?.signal))[0] : undefined
      if (!valid()) return
      publish(latest ?? { ...result, binding_revision: bindingRevision.value })
      if (result.error) error.value = t(balanceErrorKey(result))
    }
  })
}
async function testWallet() {
  const account = props.account, config = walletConfig.value, url = normalizedURL.value
  if (!account || !config || !url || manualBound.value) return
  probe.value = undefined
  await action(async valid => {
    querying.value = true
    const result = await balanceAPI.testWallet(account.id, url, config.revision, abort?.signal)
    if (!valid()) return
    walletURL.value = result.url
    await nextTick()
    if (!valid()) return
    probe.value = result
    if (result.wallet.status === 'unsupported') error.value = t('sourcesBalance.walletNotSupported')
    else if (result.wallet.status === 'wallet_unavailable') error.value = t('sourcesBalance.walletUnavailable')
    else if (result.wallet.error === 'upstream_credential_invalid') error.value = t('sourcesBalance.walletCredentialInvalid')
    else if (result.wallet.status !== 'ready') error.value = t(balanceErrorKey(result.wallet))
  })
}
async function saveWallet(reset = false) {
  const account = props.account, config = walletConfig.value
  if (!account || !config || manualBound.value || (!reset && !canSaveWallet.value)) return
  const url = reset ? '' : probe.value!.url, token = reset ? undefined : probe.value!.verification_token
  await action(async valid => {
    // Canonical New bindings block independent Sub settings; a managed Sub
    // wallet still owns its representative's query configuration.
    const [latest] = await balanceAPI.summaries([account.id], abort?.signal)
    if (!valid()) return
    if (!latest || latest.source_id || latest.status !== 'unbound' && latest.provider !== 'sub') {
      if (latest) publish(latest)
      probe.value = undefined
      error.value = t('sourcesBalance.manualBindingFirst')
      return
    }
    const result = await balanceAPI.saveWallet(account.id, url, config.revision, token, abort?.signal)
    if (!valid()) return
    walletConfig.value = result; walletURL.value = result.url; probe.value = undefined
    await nextTick()
    if (!valid()) return
    notice.value = t(reset ? 'sourcesBalance.walletReset' : 'sourcesBalance.walletSaved')
    querying.value = true
    const automatic = await balanceAPI.automatic(account.id, true, abort?.signal)
    if (!valid()) return
    const [latestSummary] = await balanceAPI.summaries([account.id], abort?.signal)
    if (!valid()) return
    if (latestSummary?.source_id || latestSummary?.status === 'rebind_required') publish(latestSummary)
    else publish({ ...automatic, binding_revision: latestSummary?.binding_revision ?? bindingRevision.value })
  })
}
async function bind() {
  const account = props.account, source = selected.value
  if (!account || !source || !sourceVerified(source)) return
  await action(async valid => {
    const result = await balanceAPI.bind(account.id, source, bindingRevision.value)
    if (!valid()) return
    publish(result)
    notice.value = t('sourcesBalance.bound')
  })
}
async function unbind() {
  const account = props.account
  if (!account) return
  await action(async valid => {
    const result = await balanceAPI.unbind(account.id, bindingRevision.value)
    if (!valid()) return
    publish(result)
    selectedID.value = ''; notice.value = t('sourcesBalance.unboundSuccess')
    await load()
  })
}
async function refresh() {
  const source = selected.value, account = props.account
  if (!source || !account) return
  await action(async valid => {
    querying.value = true
    const value = await balanceAPI.refreshSource(source.id, abort?.signal)
    if (!valid()) return
    storeSource(value)
    const [summary] = await balanceAPI.summaries([account.id])
    if (!valid()) return
    if (summary) publish(summary)
    if (!sourceVerified(value)) { error.value = t(balanceErrorKey({ error: value.error })); return }
    notice.value = t('sourcesBalance.verified')
  })
}
async function save() {
  const account = props.account
  if (!account) return
  const id = upstreamID.value.trim()
  if (id && (!/^\d+$/.test(id) || !Number.isSafeInteger(Number(id)) || Number(id) < 1)) { error.value = t('sourcesBalance.errors.invalidName'); return }
  const token = form.value.personal_token.trim()
  if ((!editingID.value && !token) || token.startsWith('sk-') || /[\r\n]/.test(token)) { error.value = t('sourcesBalance.errors.invalidPAT'); return }
  await action(async valid => {
    const payload = { ...form.value, name: form.value.name.trim(), origin: form.value.origin.trim(),
      account_id: id ? Number(id) : 0, personal_token: token, threshold: form.value.threshold.trim(),
      threshold_unit: form.value.threshold_unit.trim() }
    const saved = await balanceAPI.saveSource(editingID.value, payload)
    form.value.personal_token = ''
    if (!valid()) return
    storeSource(saved); editor.value = false; notice.value = t('sourcesBalance.saved')
    querying.value = true
    const checked = await balanceAPI.refreshSource(saved.id, abort?.signal)
    if (!valid()) return
    storeSource(checked)
    if (!sourceVerified(checked)) { error.value = t(balanceErrorKey({ error: checked.error })); return }
    // Verification is a prerequisite, never a fake client-side association.
    const result = await balanceAPI.bind(account.id, checked, bindingRevision.value)
    if (!valid()) return
    publish(result)
    notice.value = t('sourcesBalance.bound')
  })
  form.value.personal_token = ''
}
</script>

<template>
  <BalanceSurface :embedded="embedded" :show="Boolean(account)" :title="t('sourcesBalance.column')" placement="right" :show-close-button="!busy || querying" :close-on-escape="!busy || querying" @close="emit('close')">
    <div class="space-y-5" data-testid="balance-drawer">
      <p v-if="!embedded" class="text-sm font-medium text-gray-900 dark:text-gray-100">{{ t('sourcesBalance.configureAccount', { name: account?.name }) }}</p>
      <div v-if="current?.automatic" class="rounded-xl border border-gray-200 p-4 dark:border-dark-700">
        <p class="mb-2 text-sm text-gray-600 dark:text-gray-300">{{ walletConfig?.url ? t('sourcesBalance.customActive', { url: walletConfig.url }) : t('sourcesBalance.automaticHint') }}</p>
        <AccountBalanceCell v-if="!embedded" :summary="current" :supported="supported" />

        <p v-if="!embedded" class="mt-3 text-xs leading-relaxed text-gray-500" data-testid="wallet-refresh-schedule">{{ t(current.auto_refresh ? 'sourcesBalance.autoRefreshEnabled' : 'sourcesBalance.autoRefreshAfterSuccess') }}</p>
        <p v-if="!embedded && current.next_check_at" class="mt-1 text-xs text-gray-500">{{ t('sourcesBalance.nextCheck', { time: formatDateTime(current.next_check_at) }) }}</p>
        <p v-if="current.error && current.last_attempt_at" class="mt-1 text-xs text-gray-500">{{ t('sourcesBalance.lastAttempt', { time: formatDateTime(current.last_attempt_at) }) }}</p>
        <button type="button" class="btn btn-secondary mt-3" :disabled="busy || loading" @click="refreshAutomatic">{{ t(busy ? 'sourcesBalance.testingWallet' : 'sourcesBalance.queryOnce') }}</button>
      </div>
      <div v-if="!embedded && (current?.source_id || current?.status === 'rebind_required')" class="rounded-xl border border-gray-200 bg-gray-50 p-4 dark:border-dark-700 dark:bg-dark-800">
        <p class="mb-2 text-xs text-gray-500">{{ t('sourcesBalance.boundTo', { name: current?.source_name || current?.source_id }) }}</p>
        <AccountBalanceCell :summary="current" :supported="supported" @configure="editor = false" />
        <p v-if="current?.status === 'rebind_required'" class="mt-2 text-xs text-amber-700 dark:text-amber-300">{{ t('sourcesBalance.rebindHint') }}</p>
      </div>
      <p v-if="querying" role="status" class="text-xs text-gray-500">{{ t('sourcesBalance.queryWaiting') }}</p>
      <p v-if="error" role="alert" class="rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">{{ error }}</p>
      <p v-if="notice" role="status" class="text-sm text-emerald-700 dark:text-emerald-300">{{ notice }}</p>
      <div v-if="loading" role="status" class="py-6 text-center text-sm text-gray-500">{{ t('sourcesBalance.loading') }}</div>
      <template v-else-if="supported">
        <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-800">
          <p class="text-xs text-gray-500">{{ t('sourcesBalance.modelURL') }}</p>
          <p class="mt-1 break-all text-sm text-gray-700 dark:text-gray-300">{{ walletConfig?.call_url || t('sourcesBalance.modelURLUnknown') }}</p>
        </div>
        <fieldset :disabled="busy">
          <legend class="mb-2 text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('sourcesBalance.queryMode') }}</legend>
          <div class="grid grid-cols-3 gap-1 rounded-xl bg-gray-100 p-1 dark:bg-dark-800">
          <label v-for="option in (['automatic', 'sub', 'new'] as const)" :key="option" class="flex min-h-11 cursor-pointer items-center justify-center rounded-lg px-1 text-center text-xs transition-colors focus-within:ring-2 focus-within:ring-primary-500 sm:text-sm" :class="mode === option ? 'bg-white font-medium text-primary-700 shadow-sm dark:bg-dark-700 dark:text-primary-300' : 'text-gray-500 dark:text-gray-400'">
            <input v-model="mode" type="radio" name="wallet-query-mode" :value="option" :data-testid="`balance-mode-${option}`" class="sr-only" />
            {{ t(option === 'automatic' ? 'sourcesBalance.modeAuto' : option === 'sub' ? 'sourcesBalance.modeSub' : 'sourcesBalance.modeNew') }}
          </label>
          </div>
        </fieldset>
        <div v-if="mode !== 'new' && manualBound" class="space-y-3 rounded-xl bg-amber-50 p-4 dark:bg-amber-900/20">
          <p class="text-sm text-amber-800 dark:text-amber-200">{{ t('sourcesBalance.manualBindingFirst') }}</p>
          <button type="button" class="btn btn-secondary" :disabled="busy" data-testid="unlink-before-sub" @click="unbind">{{ t('sourcesBalance.unlinkFirst') }}</button>
        </div>
        <div v-else-if="mode === 'automatic'" class="space-y-3">
          <p class="text-sm text-gray-500">{{ t('sourcesBalance.autoModeHint') }}</p>
          <template v-if="walletConfig?.url">
            <p class="text-sm text-gray-500">{{ t('sourcesBalance.autoResetHint') }}</p>
            <button type="button" class="btn btn-primary w-full" :disabled="busy" data-testid="wallet-reset" @click="saveWallet(true)">{{ t('sourcesBalance.restoreAuto') }}</button>
          </template>
        </div>
        <form v-else-if="mode === 'sub'" class="space-y-4" data-testid="wallet-form" @submit.prevent="testWallet">
          <label class="block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('sourcesBalance.walletURL') }}
            <input v-model="walletURL" class="input mt-2" type="url" required maxlength="2048" placeholder="https://sub.example.com" :disabled="busy || !walletConfig?.supported" data-testid="wallet-url" aria-describedby="wallet-url-hint" />
          </label>
          <p id="wallet-url-hint" class="text-xs leading-relaxed text-gray-500">{{ t('sourcesBalance.walletURLHint') }}</p>
          <div class="rounded-lg bg-gray-50 p-3 dark:bg-dark-800">
            <p class="text-sm text-gray-700 dark:text-gray-200">{{ t('sourcesBalance.reuseKey') }}</p>
            <p class="mt-1 text-xs text-gray-500">{{ t('sourcesBalance.reuseKeyHint') }}</p>
          </div>
          <button type="submit" class="btn btn-secondary w-full" :disabled="busy || !normalizedURL || !walletConfig?.supported" data-testid="wallet-test">{{ t(busy ? 'sourcesBalance.testingWallet' : 'sourcesBalance.testWallet') }}</button>
          <div v-if="probe?.wallet.status === 'ready' && canSaveWallet" class="space-y-2 rounded-xl border border-emerald-200 bg-emerald-50 p-4 dark:border-emerald-800 dark:bg-emerald-900/20" role="status" data-testid="wallet-result">
            <p class="text-xs text-emerald-700 dark:text-emerald-300">{{ t('sourcesBalance.walletTitle') }}</p>
            <p class="break-all text-2xl font-semibold tabular-nums text-emerald-900 dark:text-emerald-100">{{ decimalAmount(probe.wallet.remaining || '') }} <span class="text-sm font-medium">{{ probe.wallet.unit }}</span></p>
            <p class="break-all text-xs text-gray-500">{{ t('sourcesBalance.queryOrigin') }} · {{ probe.url }}</p>
            <p class="text-xs text-emerald-700 dark:text-emerald-300">{{ t('sourcesBalance.walletVerified') }}</p>
          </div>
        </form>
        <form v-else-if="editor" class="space-y-4" @submit.prevent="save">
          <button type="button" class="text-sm text-primary-600 hover:underline dark:text-primary-400" :disabled="busy" @click="editor = false; form.personal_token = ''">← {{ t('sourcesBalance.backToSources') }}</button>
          <p v-if="editingID" class="rounded-lg bg-amber-50 p-3 text-xs text-amber-800 dark:bg-amber-900/20 dark:text-amber-200">{{ shared > 1 ? t('sourcesBalance.sharedWarning', { count: shared }) : t('sourcesBalance.sharedEditWarning') }}</p>
          <label class="block text-sm text-gray-700 dark:text-gray-300">{{ t('sourcesBalance.name') }}<input v-model="form.name" class="input mt-1" required maxlength="80" :disabled="busy" /></label>
          <label class="block text-sm text-gray-700 dark:text-gray-300">{{ t('sourcesBalance.origin') }}<input v-model="form.origin" class="input mt-1" type="url" required placeholder="https://example.com" :disabled="busy || Boolean(editingID)" /><span class="mt-1 block text-xs text-gray-500">{{ t('sourcesBalance.originHint') }}</span></label>
          <label class="block text-sm text-gray-700 dark:text-gray-300">{{ t('sourcesBalance.pat') }}<input v-model="form.personal_token" class="input mt-1" type="password" autocomplete="new-password" :required="!editingID" :placeholder="editingID ? t('sourcesBalance.keepPAT') : ''" :disabled="busy" /><span class="mt-1 block text-xs text-gray-500">{{ t('sourcesBalance.patHint') }}</span></label>
          <label class="block text-sm text-gray-700 dark:text-gray-300">{{ t('sourcesBalance.upstreamUser') }}<input v-model="upstreamID" class="input mt-1" inputmode="numeric" :disabled="busy || Boolean(editingExisting?.account_id)" /><span class="mt-1 block text-xs text-gray-500">{{ t('sourcesBalance.accountIDHint') }}</span></label>
          <p class="text-xs text-gray-500">{{ t('sourcesBalance.management.queryAlertHint') }}</p>
          <p v-if="editingID" class="text-xs text-gray-500">{{ t('sourcesBalance.immutableOrigin') }}</p>
          <button type="submit" class="btn btn-primary w-full" :disabled="busy">{{ t(busy ? 'sourcesBalance.verifying' : 'sourcesBalance.saveAndVerify') }}</button>
        </form>
        <div v-else-if="mode === 'new'" class="space-y-4">
          <label class="block text-sm font-medium text-gray-700 dark:text-gray-300">{{ t('sourcesBalance.existing') }}
            <select v-model="selectedID" class="input mt-2" :disabled="busy" data-testid="balance-source-select">
              <option value="">{{ t('sourcesBalance.selectPlaceholder') }}</option>
              <option v-for="source in sources" :key="source.id" :value="source.id">{{ source.name }} · {{ source.origin }}</option>
            </select>
          </label>
          <p class="text-xs leading-relaxed text-gray-500">{{ t(sources.length ? 'sourcesBalance.sourceHint' : 'sourcesBalance.noSources') }}</p>
          <div v-if="selected" class="space-y-3 rounded-xl border border-gray-200 p-4 dark:border-dark-700">
            <div class="break-all text-xs text-gray-500">{{ selected.origin }} · #{{ selected.account_id || '—' }}</div>
            <p v-if="selected.last_balance" class="text-lg font-semibold tabular-nums text-gray-900 dark:text-gray-100">{{ decimalAmount(selected.last_balance.remaining) }} <span class="text-sm font-normal">{{ selected.last_balance.unit || t('sourcesBalance.unitUnknown') }}</span></p>
            <p v-if="selected.last_balance" class="text-xs text-gray-500">{{ t('sourcesBalance.lastSuccess', { time: formatDateTime(selected.last_balance.checked_at) }) }}</p>
            <p v-if="!verified" class="text-xs text-amber-700 dark:text-amber-300">{{ t(selected.last_balance && selected.status === 'error' ? 'sourcesBalance.sourceFailure' : 'sourcesBalance.unverified') }}</p>
            <div class="flex flex-wrap gap-3">
              <button type="button" class="text-sm text-primary-600 hover:underline dark:text-primary-400" :disabled="busy" @click="refresh">{{ t('sourcesBalance.queryOnce') }}</button>
              <button type="button" class="text-sm text-primary-600 hover:underline dark:text-primary-400" :disabled="busy" @click="resetEditor(selected)">{{ t('sourcesBalance.editSource') }}</button>
            </div>
          </div>
          <button type="button" class="btn btn-primary w-full" :disabled="busy || !verified" @click="bind">{{ t(current?.source_id ? 'sourcesBalance.replaceBinding' : 'sourcesBalance.bind') }}</button>
          <button type="button" class="btn btn-secondary w-full" :disabled="busy" @click="resetEditor()">{{ t('sourcesBalance.newSource') }}</button>
          <button v-if="current?.source_id" type="button" class="w-full py-2 text-sm text-gray-500 hover:text-red-600 dark:hover:text-red-400" :disabled="busy" @click="unbind">{{ t('sourcesBalance.unlinkFirst') }}</button>
        </div>
      </template>
      <p v-else class="text-sm text-gray-500">{{ t('sourcesBalance.unsupported') }}</p>
      <button v-if="!loading && error" type="button" class="text-sm text-primary-600 dark:text-primary-400" :disabled="busy" @click="load">{{ t('sourcesBalance.loadRetry') }}</button>
    </div>
    <template #footer>
      <div class="flex w-full items-center justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="busy && !querying" @click="emit('close')">{{ t('sourcesBalance.cancel') }}</button>
        <button v-if="mode === 'sub' && !manualBound && supported" type="button" class="btn btn-primary" :disabled="busy || loading || !canSaveWallet" data-testid="wallet-save" @click="saveWallet()">{{ t('sourcesBalance.saveWallet') }}</button>
      </div>
    </template>
  </BalanceSurface>
</template>
