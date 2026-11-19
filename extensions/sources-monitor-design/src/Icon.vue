<template><svg :width="size" :height="size" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path :d="paths[name] || paths.grid" /></svg></template>
<script setup lang="ts">
withDefaults(defineProps<{ name: string; size?: number }>(), { size: 19 })
const paths: Record<string, string> = {
  grid: 'M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z',
  activity: 'M2 12h4l3-8 6 16 3-8h4',
  key: 'M14 4a6 6 0 1 1-4 10L3 21H1v-4l7-7a6 6 0 0 1 6-6z M16 7h.01',
  chart: 'M4 20V10h4v10z M10 20V4h4v16z M16 20v-9h4v9z',
  layers: 'm12 3 10 5-10 5L2 8z M2 12l10 5 10-5 M2 16l10 5 10-5',
  wallet: 'M20 8V5H5a3 3 0 0 0 0 6h17v9H5a3 3 0 0 1-3-3V8 M22 12h-6v5h6 M18 14.5h.01',
  book: 'M12 5c-3-2-7-2-10-1v15c3-1 7-1 10 1 3-2 7-2 10-1V4c-3-1-7-1-10 1z M12 5v15',
  sun: 'M12 8a4 4 0 1 1 0 8 4 4 0 0 1 0-8 M12 2v2 M12 20v2 M2 12h2 M20 12h2 M5 5l1 1 M18 18l1 1 M5 19l1-1 M18 6l1-1',
  moon: 'M20 15A9 9 0 0 1 9 4a9 9 0 1 0 11 11z',
  chevron: 'm9 5 7 7-7 7',
  down: 'm6 9 6 6 6-6',
  search: 'M10.5 3a7.5 7.5 0 1 0 0 15 7.5 7.5 0 0 0 0-15 M16 16l5 5',
  info: 'M12 2a10 10 0 1 0 0 20 10 10 0 0 0 0-20 M12 11v6 M12 7h.01',
  close: 'm6 6 12 12 M6 18 18 6',
  refresh: 'M20 7a9 9 0 1 0 1 9 M20 2v6h-6',
  external: 'M14 3h7v7 M21 3l-9 9 M10 3H4v17h17v-6',
  bell: 'M18 8a6 6 0 0 0-12 0c0 7-3 8-3 8h18s-3-1-3-8 M10 20h4',
  menu: 'M4 6h16 M4 12h16 M4 18h16',
  filter: 'M3 5h18 M6 12h12 M10 19h4',
  check: 'm5 12 4 4L19 6',
}
</script>
