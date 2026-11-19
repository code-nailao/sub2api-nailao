<template>
  <div class="bg-gray-50 dark:bg-dark-950" :class="mode === 'default' ? 'min-h-screen' : 'h-dvh overflow-hidden'">
    <!-- Background Decoration -->
    <div class="pointer-events-none fixed inset-0 bg-mesh-gradient"></div>

    <!-- Sidebar -->
    <AppSidebar v-if="mode !== 'studio-standalone'" />

    <!-- Main Content Area -->
    <div
      class="relative transition-all duration-300"
      :class="[mode === 'default' ? 'min-h-screen' : 'h-full', mode === 'studio-standalone' ? '' : sidebarCollapsed ? 'lg:ml-[72px]' : 'lg:ml-64']"
    >
      <!-- Header -->
      <AppHeader v-if="mode !== 'studio-standalone'" :hide-page-title="mode === 'studio'" />

      <!-- Main Content -->
      <main :class="mode === 'default' ? 'p-4 md:p-6 lg:p-8' : mode === 'studio-standalone' ? 'h-full overflow-hidden' : 'h-[calc(100dvh-64px)] overflow-hidden'">
        <slot />
      </main>
    </div>
  </div>
</template>

<script setup lang="ts">
import '@/styles/onboarding.css'
import { computed, onMounted } from 'vue'
import { useAppStore } from '@/stores'
import { useAuthStore } from '@/stores/auth'
import { useOnboardingTour } from '@/composables/useOnboardingTour'
import { useOnboardingStore } from '@/stores/onboarding'
import AppSidebar from './AppSidebar.vue'
import AppHeader from './AppHeader.vue'

const props = withDefaults(defineProps<{ mode?: 'default' | 'studio' | 'studio-standalone' }>(), { mode: 'default' })

const appStore = useAppStore()
const authStore = useAuthStore()
const sidebarCollapsed = computed(() => appStore.sidebarCollapsed)
const isAdmin = computed(() => authStore.user?.role === 'admin')

const { replayTour } = useOnboardingTour({
  storageKey: isAdmin.value ? 'admin_guide' : 'user_guide',
  autoStart: props.mode === 'default'
})

const onboardingStore = useOnboardingStore()

onMounted(() => {
  onboardingStore.setReplayCallback(replayTour)
})

defineExpose({ replayTour })
</script>
