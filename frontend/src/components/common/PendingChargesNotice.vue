<template>
  <section
    v-if="!unsupported && (loading || error || items.length > 0)"
    data-testid="pending-charges-notice"
    class="card border-amber-200 p-4 dark:border-amber-700/50"
    :aria-labelledby="titleId"
    :aria-busy="loading"
  >
    <div class="flex flex-wrap items-center justify-between gap-3">
      <div class="min-w-0">
        <h2 :id="titleId" class="text-sm font-medium text-gray-900 dark:text-gray-100">
          {{ t('pendingCharges.title') }}
        </h2>
        <p v-if="items.length" class="mt-1 text-sm text-amber-800 dark:text-amber-200" aria-live="polite">
          {{ t('pendingCharges.loadedSummary', { count: items.length, amount: formatAmount(loadedAmount) }) }}
        </p>
        <p v-else-if="loading" class="mt-1 text-sm text-gray-600 dark:text-gray-400" role="status">
          {{ t('pendingCharges.loading') }}
        </p>
      </div>
      <button
        v-if="items.length"
        type="button"
        class="btn btn-secondary min-h-11"
        :aria-expanded="expanded"
        :aria-controls="detailsId"
        @click="expanded = !expanded"
      >
        {{ t(expanded ? 'pendingCharges.hideDetails' : 'pendingCharges.showDetails') }}
      </button>
    </div>

    <p v-if="items.length" class="mt-3 text-sm text-gray-600 dark:text-gray-400">
      {{ t('pendingCharges.hint') }}
    </p>
    <p v-if="hasUnknown" class="mt-3 text-sm text-amber-800 dark:text-amber-200">
      {{ t('pendingCharges.unknownHint') }}
    </p>

    <div v-if="error" class="mt-3 flex flex-wrap items-center gap-3">
      <p class="text-sm text-red-700 dark:text-red-300" role="alert">
        {{ t(error === 'more' ? 'pendingCharges.moreError' : 'pendingCharges.readError') }}
      </p>
      <button type="button" class="btn btn-secondary min-h-11" :disabled="loading" @click="load(error === 'more')">
        {{ t('pendingCharges.retry') }}
      </button>
    </div>

    <div v-if="items.length" v-show="expanded" :id="detailsId" class="mt-4 space-y-3">
      <p class="text-sm text-gray-600 dark:text-gray-400">{{ t('pendingCharges.filtersHint') }}</p>
      <ul class="divide-y divide-gray-200 dark:divide-dark-600">
        <li v-for="item in items" :key="item.request_id" class="py-3" data-testid="pending-charge-row">
          <dl class="grid grid-cols-1 gap-x-6 gap-y-2 text-sm sm:grid-cols-2 lg:grid-cols-3">
            <div class="min-w-0 sm:col-span-2 lg:col-span-1">
              <dt class="text-gray-500 dark:text-gray-400">{{ t('pendingCharges.requestId') }}</dt>
              <dd class="break-all font-mono text-gray-900 dark:text-gray-100">{{ item.request_id }}</dd>
            </div>
            <div class="min-w-0">
              <dt class="text-gray-500 dark:text-gray-400">{{ t('pendingCharges.model') }}</dt>
              <dd class="break-words text-gray-900 dark:text-gray-100">{{ item.model }}</dd>
            </div>
            <div>
              <dt class="text-gray-500 dark:text-gray-400">{{ t('pendingCharges.createdAt') }}</dt>
              <dd class="text-gray-900 dark:text-gray-100"><time :datetime="item.created_at">{{ formatDate(item.created_at) }}</time></dd>
            </div>
            <div>
              <dt class="text-gray-500 dark:text-gray-400">{{ t('pendingCharges.state') }}</dt>
              <dd class="text-gray-900 dark:text-gray-100">{{ t(`pendingCharges.${item.state}`) }}</dd>
            </div>
            <div>
              <dt class="text-gray-500 dark:text-gray-400">{{ t('pendingCharges.heldPrice') }}</dt>
              <dd class="tabular-nums text-gray-900 dark:text-gray-100">¥{{ formatAmount(item.held_price) }}</dd>
            </div>
          </dl>
        </li>
      </ul>
      <div class="flex flex-wrap items-center justify-between gap-3">
        <p class="text-sm text-gray-600 dark:text-gray-400">
          {{ t(nextCursor ? 'pendingCharges.moreAvailable' : 'pendingCharges.allLoaded') }}
        </p>
        <div class="flex flex-wrap gap-2">
          <button type="button" class="btn btn-secondary min-h-11" :disabled="loading" @click="load(false)">
            {{ t('pendingCharges.refresh') }}
          </button>
          <button v-if="nextCursor" type="button" class="btn btn-secondary min-h-11" :disabled="loading" @click="load(true)">
            {{ t(loading ? 'pendingCharges.loadingMore' : 'pendingCharges.loadMore') }}
          </button>
        </div>
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, useId } from 'vue'
import { useI18n } from 'vue-i18n'
import { listPendingCharges, type PendingCharge } from '@/api/pendingCharges'
import { formatDate } from '@/utils/format'

const { t } = useI18n()
const titleId = useId()
const detailsId = useId()
const items = ref<PendingCharge[]>([])
const nextCursor = ref<string | null>(null)
const expanded = ref(false)
const loading = ref(false)
const unsupported = ref(false)
const error = ref<'first' | 'more' | null>(null)
const loadedAmount = computed(() => items.value.reduce((sum, item) => sum + item.held_price, 0))
const hasUnknown = computed(() => items.value.some(item => item.state === 'unknown'))
let controller: AbortController | undefined

const amountFormatter = new Intl.NumberFormat('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 8 })
const formatAmount = (amount: number) => amountFormatter.format(amount)

async function load(more = false) {
  if (loading.value || (more && !nextCursor.value)) return
  loading.value = true
  error.value = null
  controller = new AbortController()
  const signal = controller.signal
  try {
    const page = await listPendingCharges({
      page_size: 20,
      ...(more && nextCursor.value ? { cursor: nextCursor.value } : {}),
    }, { signal })
    if (signal.aborted) return
    // Records may change while paging; retain one row per request.
    const merged = new Map((more ? items.value : []).map(item => [item.request_id, item]))
    for (const item of page.items) merged.set(item.request_id, item)
    items.value = [...merged.values()]
    nextCursor.value = page.next_cursor
  } catch (cause) {
    if (signal.aborted) return
    const status = (cause as { status?: number; response?: { status?: number } } | null)?.status
      ?? (cause as { response?: { status?: number } } | null)?.response?.status
    if (status === 404) {
      unsupported.value = true
      items.value = []
    } else {
      error.value = more ? 'more' : 'first'
    }
  } finally {
    if (!signal.aborted) loading.value = false
  }
}

onMounted(() => load())
onBeforeUnmount(() => controller?.abort())
</script>
