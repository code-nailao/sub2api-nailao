<template>
  <BaseDialog
    :show="show"
    :title="t('admin.channelMonitor.sortOrder')"
    :show-close-button="!submitting"
    :close-on-escape="!submitting"
    @close="emit('close')"
  >
    <p class="mb-4 text-sm text-gray-500 dark:text-gray-400">{{ t('admin.channelMonitor.sortOrderHint') }}</p>
    <VueDraggable v-model="localItems" :animation="200" :disabled="submitting" handle=".monitor-drag-handle" class="space-y-2">
      <div v-for="(item, index) in localItems" :key="item.id" :data-monitor-id="item.id" class="flex items-center gap-3 rounded-lg border border-gray-200 p-3 dark:border-dark-600">
        <span class="monitor-drag-handle cursor-grab touch-none text-gray-400"><Icon name="menu" size="md" /></span>
        <div class="min-w-0 flex-1">
          <div class="truncate font-medium text-gray-900 dark:text-white">{{ item.name }}</div>
          <div class="truncate text-xs text-gray-500 dark:text-gray-400">{{ item.primary_model }} · {{ item.enabled ? t('common.enabled') : t('common.disabled') }}</div>
        </div>
        <button type="button" class="btn btn-secondary p-1.5" :disabled="submitting || index === 0" :aria-label="t('admin.channelMonitor.moveUp', { name: item.name })" @click="move(index, -1)"><Icon name="arrowUp" size="sm" /></button>
        <button type="button" class="btn btn-secondary p-1.5" :disabled="submitting || index === localItems.length - 1" :aria-label="t('admin.channelMonitor.moveDown', { name: item.name })" @click="move(index, 1)"><Icon name="arrowDown" size="sm" /></button>
      </div>
    </VueDraggable>
    <template #footer>
      <div class="flex justify-end gap-3">
        <button type="button" class="btn btn-secondary" :disabled="submitting" @click="emit('close')">{{ t('common.cancel') }}</button>
        <button type="button" class="btn btn-primary" :disabled="submitting || localItems.length === 0" @click="emit('save', localItems)">{{ submitting ? t('common.saving') : t('common.save') }}</button>
      </div>
    </template>
  </BaseDialog>
</template>

<script setup lang="ts">
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { VueDraggable } from 'vue-draggable-plus'
import type { ChannelMonitor } from '@/api/admin/channelMonitor'
import BaseDialog from '@/components/common/BaseDialog.vue'
import Icon from '@/components/icons/Icon.vue'

const props = defineProps<{ show: boolean; items: ChannelMonitor[]; submitting: boolean }>()
const emit = defineEmits<{ close: []; save: [items: ChannelMonitor[]] }>()
const { t } = useI18n()
const localItems = ref<ChannelMonitor[]>([])
watch(() => [props.show, props.items] as const, ([show]) => {
  if (show) localItems.value = [...props.items]
}, { immediate: true })
function move(index: number, direction: number) {
  if (props.submitting) return
  const target = index + direction
  if (target < 0 || target >= localItems.value.length) return
  const items = [...localItems.value]
  ;[items[index], items[target]] = [items[target], items[index]]
  localItems.value = items
}
</script>
