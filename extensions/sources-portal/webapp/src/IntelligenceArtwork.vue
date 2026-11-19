<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { schedulePreview } from './intelligence-preview-queue'
import { artworkDocument, initialArtworkSize, validArtworkSize, type ArtworkSize } from './intelligence-artwork'

const props = withDefaults(defineProps<{ html?: string; load?: () => Promise<string>; mode?: 'fit' | 'actual'; thumbnail?: boolean; suspended?: boolean; label?: string }>(), { html: '', mode: 'fit', thumbnail: false, suspended: false, label: '鹈鹕作品' })
const emit = defineEmits<{ size: [value: ArtworkSize] }>()
const viewport = ref<HTMLElement>(), frame = ref<HTMLIFrameElement>()
const visible = ref(!props.thumbnail), size = ref<ArtworkSize>({ ...initialArtworkSize })
const area = ref({ width: 0, height: 0 }), source = ref(''), content = ref(props.html), error = ref(''), ready = ref(false)
let generation = 0
let channel = '', observer: IntersectionObserver | undefined, resize: ResizeObserver | undefined
let acceptedSizes = 0, cancelCreate: (() => void) | undefined, exitTimer: ReturnType<typeof setTimeout> | undefined
const eligible = computed(() => visible.value && !props.suspended)
const scale = computed(() => props.mode === 'actual' ? 1 : Math.max(.01, Math.min((area.value.width - 2) / size.value.width, (area.value.height - 2) / size.value.height)))
const stageStyle = computed(() => ({
  width: size.value.width * scale.value + 'px',
  height: size.value.height * scale.value + 'px',
  marginLeft: Math.max(0, (area.value.width - size.value.width * scale.value) / 2) + 'px',
  marginTop: Math.max(0, (area.value.height - size.value.height * scale.value) / 2) + 'px',
}))
const frameStyle = computed(() => ({ width: size.value.width + 'px', height: size.value.height + 'px', transform: 'scale(' + scale.value + ')' }))

function clear() {
  cancelCreate?.(); cancelCreate = undefined
  if (source.value) URL.revokeObjectURL(source.value)
  source.value = ''
  ready.value = false
}
async function render() {
  const current = ++generation
  clear()
  error.value = ''
  content.value = props.html
  size.value = { ...initialArtworkSize }; acceptedSizes = 0
  emit('size', size.value)
  if (!eligible.value) { content.value = ''; return }
  try {
    if (!content.value && props.load) content.value = await props.load()
    if (current !== generation || !eligible.value || !content.value) return
    cancelCreate = schedulePreview(() => {
      if (current !== generation || !eligible.value) return
      channel = crypto.randomUUID()
      source.value = URL.createObjectURL(new Blob([artworkDocument(content.value, channel)], { type: 'text/html' }))
    })
  } catch { if (current === generation) error.value = '作品预览暂不可用' }
}
function receive(event: MessageEvent) {
  if (event.source !== frame.value?.contentWindow || event.origin !== 'null' ||
      event.data?.type !== 'sources:intelligence-artwork-size' || event.data.channel !== channel ||
      !validArtworkSize(event.data) || acceptedSizes >= 8) return
  acceptedSizes++
  size.value = { width: event.data.width, height: event.data.height }
  emit('size', size.value)
}
function measure() {
  if (viewport.value) area.value = { width: viewport.value.clientWidth, height: viewport.value.clientHeight }
}
watch(() => [props.html, eligible.value], render)
watch(() => props.mode, async () => { await nextTick(); viewport.value?.scrollTo(0, 0); measure() })
onMounted(() => {
  resize = new ResizeObserver(measure)
  if (viewport.value) resize.observe(viewport.value)
  if (props.thumbnail) {
    observer = new IntersectionObserver(entries => {
      clearTimeout(exitTimer)
      if (entries[0]?.isIntersecting) visible.value = true
      else exitTimer = setTimeout(() => { visible.value = false }, 150)
    }, { rootMargin: '0px' })
    if (viewport.value) observer.observe(viewport.value)
  }
  window.addEventListener('message', receive)
  measure(); render()
})
onBeforeUnmount(() => { generation++; clearTimeout(exitTimer); observer?.disconnect(); resize?.disconnect(); window.removeEventListener('message', receive); clear() })
</script>

<template>
  <div ref="viewport" class="art-viewport" :class="{ 'art-viewport-actual': mode === 'actual', 'art-viewport-thumbnail': thumbnail }">
    <div v-if="source" class="art-stage" :class="{ 'art-stage-loading':!ready }" :style="stageStyle">
      <iframe ref="frame" :src="source" :style="frameStyle" :title="label" sandbox="allow-scripts" referrerpolicy="no-referrer" :tabindex="thumbnail ? -1 : 0" @load="ready=true"></iframe>
    </div>
    <div v-if="!source || !ready" class="art-placeholder" :class="{ 'art-loading-overlay':source }">{{ error || (visible ? '正在读取作品…' : '作品预览') }}</div>
  </div>
</template>
