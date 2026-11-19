<script setup lang="ts">
import BaseDialog from '@/components/common/BaseDialog.vue'
defineOptions({ inheritAttrs: false })
defineProps<{ embedded?: boolean; show: boolean; title: string }>()
</script>
<template>
  <section v-if="embedded" class="space-y-5">
    <slot />
    <div class="flex flex-wrap justify-end gap-2 border-t border-gray-100 pt-4 dark:border-dark-700"><slot name="footer" /></div>
  </section>
  <BaseDialog v-else :show="show" :title="title" v-bind="$attrs"><slot /><template #footer><slot name="footer" /></template></BaseDialog>
</template>
