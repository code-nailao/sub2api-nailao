<template>
  <article class="sources-channel-card glass-card" :data-state="state">
    <header class="channel-card-heading">
      <span class="provider-mark" :class="providerClass">
        <span v-if="category === 'domestic' && row.platform === 'openai'" class="domestic-mark" aria-hidden="true"><ProviderIcon provider="deepseek" :size="22" /><ProviderIcon provider="kimi" :size="13" /></span>
        <ProviderIcon v-else :provider="row.platform" :size="24" />
      </span>
      <div class="channel-identity"><h3 :title="row.group_name">{{ row.group_name || `#${row.group_id}` }}</h3><div class="channel-tags"><span>{{ providerLabel }}</span><span class="rate-tag" :title="t('channelMonitorV2.cards.rateHint')">{{ t('channelMonitorV2.cards.userRate') }} <b>{{ formatMultiplier(multiplier) }}</b></span></div></div>
      <span class="state-pill" :class="`state-${state}`"><i aria-hidden="true"></i>{{ stateLabel(state) }}</span>
    </header>

    <dl class="channel-metrics">
      <div><dt>{{ t('channelMonitorV2.metrics.cacheRate') }}</dt><dd>{{ percent(cacheRate(row.metrics, row.health)) }}</dd></div>
      <div><dt>{{ t('channelMonitorV2.cards.availability') }}</dt><dd :class="`text-state-${row.health.error_rate}`">{{ percent(availability(row.metrics, row.health)) }}</dd></div>
      <div><dt>{{ t('channelMonitorV2.metrics.ttft') }} <small>P50</small></dt><dd :class="`text-state-${row.metrics.ttft.p50_ms == null ? 'unknown' : row.health.ttft}`">{{ formatMonitorMs(row.metrics.ttft.p50_ms) }}</dd></div>
    </dl>

    <div class="timeline-heading"><span>{{ t('channelMonitorV2.cards.periods', { count: buckets.length }) }}</span><span>{{ bucketLabel }}</span></div>
    <div class="channel-timeline" role="group" :aria-label="t('channelMonitorV2.cards.timelineAria', { name: row.group_name })" @mouseleave="hovered = null">
      <button
        v-for="(bucket, index) in buckets" :key="bucket.bucket_start"
        type="button" class="timeline-target" :class="`state-${cardHealth(bucket.metrics, bucket.health)}`"
        :tabindex="index === focused ? 0 : -1" :aria-label="tooltip(bucket)"
        @mouseenter="hovered = index" @focus="hovered = index; focused = index" @blur="hovered = null"
        @keydown="navigate($event, index)" @click="selectedBucket = bucket"
      ><span></span></button>
      <div v-if="!buckets.length" class="empty-timeline" aria-hidden="true"><span v-for="i in 18" :key="i"></span></div>
      <div v-if="hovered !== null && buckets[hovered]" class="channel-tooltip" role="tooltip"><strong>{{ formatTime(buckets[hovered].bucket_start) }}</strong><p>{{ tooltipMetrics(buckets[hovered]) }}</p></div>
    </div>
    <div class="timeline-axis"><span>{{ coverage ? formatTime(coverage.requested_start) : '—' }}</span><span>{{ coverage ? formatTime(coverage.requested_end || coverage.data_through) : '—' }}</span></div>

    <BaseDialog :show="selectedBucket !== null" :title="row.group_name || `#${row.group_id}`" @close="selectedBucket = null">
      <template v-if="selectedBucket"><p class="mb-5 text-sm text-gray-500 dark:text-gray-400">{{ formatTime(selectedBucket.bucket_start) }} · {{ stateLabel(cardHealth(selectedBucket.metrics, selectedBucket.health)) }}</p><dl class="channel-metrics"><div><dt>{{ t('channelMonitorV2.metrics.cacheRate') }}</dt><dd>{{ percent(cacheRate(selectedBucket.metrics, selectedBucket.health)) }}</dd></div><div><dt>{{ t('channelMonitorV2.cards.availability') }}</dt><dd>{{ percent(availability(selectedBucket.metrics, selectedBucket.health)) }}</dd></div><div><dt>{{ t('channelMonitorV2.metrics.ttftP50') }}</dt><dd>{{ formatMonitorMs(selectedBucket.metrics.ttft.p50_ms) }}</dd></div></dl><p class="mt-5 text-xs leading-relaxed text-gray-500 dark:text-gray-400">{{ t(isAdmin ? 'channelMonitorV2.cards.metricHint' : 'channelMonitorV2.cards.shortMetricHint') }}</p></template>
    </BaseDialog>
  </article>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import type { HealthState, MonitorCoverage, MonitorMatrixBucket, MonitorMatrixRow } from '@/api/channelMonitorV2'
import ProviderIcon from '@/components/user/monitor/ProviderIcon.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import { formatMonitorMs, formatMonitorPercent } from '@/features/channel-monitor-v2/monitorFormat'
import { alignedBuckets, availability, cacheRate, cardHealth, formatMultiplier, type DisplayCategory } from './display'

const props = defineProps<{ row: MonitorMatrixRow; category: DisplayCategory; multiplier?: number; isAdmin?: boolean; coverage: MonitorCoverage | null }>()
const { t, locale } = useI18n()
const hovered = ref<number | null>(null)
const focused = ref(0)
const selectedBucket = ref<MonitorMatrixBucket | null>(null)
const state = computed(() => cardHealth(props.row.metrics, props.row.health))
const buckets = computed(() => alignedBuckets(props.row.buckets, props.coverage))
watch(() => buckets.value.length, length => { focused.value = Math.max(0, Math.min(focused.value, length - 1)); hovered.value = null; selectedBucket.value = null })
const providerClass = computed(() => props.category === 'other' ? 'other' : props.category)
const providerLabel = computed(() => props.category === 'domestic' && props.row.platform === 'openai' ? t('channelMonitorV2.cards.domestic') : ({ openai: 'OpenAI', anthropic: 'Anthropic', grok: 'Grok', gemini: 'Gemini', deepseek: 'DeepSeek', kimi: 'Kimi', zhipu: 'GLM', kiro: 'Kiro', antigravity: 'Antigravity', minimax: 'MiniMax' }[props.row.platform] || props.row.platform))
const bucketLabel = computed(() => {
  const seconds = props.coverage?.bucket_seconds || 0
  if (seconds >= 86400) return t('channelMonitorV2.bucket.days', { count: seconds / 86400 })
  if (seconds >= 3600) return t('channelMonitorV2.bucket.hours', { count: seconds / 3600 })
  return seconds ? t('channelMonitorV2.bucket.minutes', { count: seconds / 60 }) : '—'
})
const percent = (value: number | null) => value == null ? '—' : formatMonitorPercent(value)
const stateLabel = (value: HealthState) => t(`channelMonitorV2.cards.states.${value}`)
function formatTime(value: string) {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? '—' : new Intl.DateTimeFormat(locale.value, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false }).format(date)
}
function tooltipMetrics(bucket: MonitorMatrixBucket) {
  return `${stateLabel(cardHealth(bucket.metrics, bucket.health))} · ${t('channelMonitorV2.cards.availability')} ${percent(availability(bucket.metrics, bucket.health))} · ${t('channelMonitorV2.metrics.cacheRate')} ${percent(cacheRate(bucket.metrics, bucket.health))} · ${t('channelMonitorV2.metrics.ttft')} ${formatMonitorMs(bucket.metrics.ttft.p50_ms)}`
}
function tooltip(bucket: MonitorMatrixBucket) { return `${formatTime(bucket.bucket_start)} · ${tooltipMetrics(bucket)}` }
function navigate(event: KeyboardEvent, index: number) {
  if (event.key === 'Escape') { hovered.value = null; return }
  const target = event.currentTarget as HTMLButtonElement
  let next: Element | null = null
  if (event.key === 'ArrowRight') next = target.nextElementSibling
  if (event.key === 'ArrowLeft') next = target.previousElementSibling
  if (event.key === 'Home') next = target.parentElement?.querySelector('button') || null
  if (event.key === 'End') next = target.parentElement?.querySelector('button:last-of-type') || null
  if (next instanceof HTMLButtonElement) { event.preventDefault(); next.focus() }
  else if (event.key === 'ArrowRight' && index === buckets.value.length - 1 || event.key === 'ArrowLeft' && index === 0) event.preventDefault()
}
</script>

<style scoped>
.sources-channel-card{position:relative;min-width:0;padding:20px;border-radius:22px;border:1px solid rgb(229 231 235 / .85);background:rgb(255 255 255 / .82);transition:border-color .18s ease,box-shadow .18s ease}
.sources-channel-card:hover{z-index:2;border-color:#a7b5c6;box-shadow:0 5px 22px rgb(0 0 0 / .06)}
.channel-card-heading{display:flex;align-items:flex-start;gap:11px;min-height:80px}.provider-mark{display:grid;place-items:center;flex-shrink:0;width:40px;height:40px;border-radius:13px;background:#eef2f6;color:#475569}.provider-mark.openai{background:#e9f7f2;color:#147b60}.provider-mark.anthropic{background:#fbf0e7;color:#ac6b3f}.provider-mark.gemini{background:#eef0ff;color:#5467bb}.provider-mark.domestic{background:#eaf2ff;color:#426cd1}.domestic-mark{display:flex;align-items:center;gap:1px}.channel-identity{min-width:0;flex:1}.channel-identity h3{margin:0;font-size:15px;line-height:1.5;font-weight:650;display:-webkit-box;-webkit-box-orient:vertical;-webkit-line-clamp:2;overflow:hidden;overflow-wrap:anywhere;min-height:45px;color:#172032}.channel-tags{display:flex;flex-wrap:wrap;gap:5px 8px;margin-top:6px;font-size:11px;color:#64748b}.rate-tag{display:inline-flex;align-items:center;gap:5px;border-radius:5px;padding:1px 5px;background:#f1f5f9}.rate-tag b{font-family:ui-monospace,SFMono-Regular,monospace;font-size:11px;font-weight:600;color:#334155}.state-pill{display:inline-flex;align-items:center;gap:4px;flex-shrink:0;font-size:10px;font-weight:600;padding:4px 6px;border-radius:20px}.state-pill i{width:4px;height:4px;border-radius:50%;background:currentColor}.state-pill.state-healthy{color:#08744c;background:#e6f7ef}.state-pill.state-warning{color:#916008;background:#fff4d8}.state-pill.state-critical{color:#b33247;background:#fff0f2}.state-pill.state-unknown{color:#64748b;background:#f1f5f9}
.channel-metrics{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:8px;margin:20px 0 18px}.channel-metrics>div{background:#f8fafc;border:1px solid #e9edf3;border-radius:12px;padding:12px 10px}.channel-metrics dt{font-size:11px;line-height:1.4;color:#64748b;white-space:nowrap}.channel-metrics dt small{font-size:9px;color:#94a3b8}.channel-metrics dd{margin:10px 0 0;font-family:ui-monospace,SFMono-Regular,Menlo,monospace;font-size:21px;font-weight:650;letter-spacing:-.7px;line-height:1.2;color:#1e293b;font-variant-numeric:tabular-nums}.channel-metrics dd.text-state-healthy{color:#087a58}.channel-metrics dd.text-state-warning{color:#a46d07}.channel-metrics dd.text-state-critical{color:#c13e53}.channel-metrics dd.text-state-unknown{color:#94a3b8}.timeline-heading,.timeline-axis{display:flex;justify-content:space-between;gap:8px;font-size:10px;color:#64748b}.timeline-heading{border-top:1px solid #edf0f4;padding-top:14px}.timeline-axis{margin-top:4px;font-variant-numeric:tabular-nums}.channel-timeline{display:flex;align-items:end;position:relative;gap:3px;height:41px;margin-top:1px}.timeline-target{display:flex;align-items:center;width:0;min-width:0;flex:1;height:40px;border-radius:4px;cursor:pointer;outline-offset:2px}.timeline-target>span{width:100%;height:22px;border-radius:3px;background:#a2aab6;transition:height .12s,filter .12s}.timeline-target.state-healthy>span{background:#21b88c}.timeline-target.state-warning>span{height:16px;background:#e5ab26}.timeline-target.state-critical>span{height:11px;background:#eb6678}.timeline-target.state-unknown>span{height:6px;background:#a2aab6}.timeline-target:hover>span,.timeline-target:focus-visible>span{filter:brightness(1.15)}.timeline-target:focus-visible{outline:2px solid #c88e21}.channel-tooltip{position:absolute;bottom:47px;left:0;right:0;z-index:15;border-radius:10px;padding:10px 12px;background:#111b2d;color:#f1f5f9;box-shadow:0 6px 24px #0003;pointer-events:none;font-size:11px;line-height:1.7}.channel-tooltip p{margin:4px 0 0;color:#cbd5e1}.empty-timeline{display:flex;width:100%;gap:3px;align-items:center;height:40px}.empty-timeline span{flex:1;height:6px;background:#cbd5e1;border-radius:3px}
.dark .sources-channel-card{background:rgb(30 41 59 / .78);border-color:rgb(71 85 105 / .45)}.dark .sources-channel-card:hover{border-color:rgb(100 116 139 / .8)}.dark .channel-identity h3{color:#f1f5f9}.dark .channel-tags,.dark .timeline-heading,.dark .timeline-axis,.dark .channel-metrics dt{color:#a3b0c2}.dark .rate-tag{background:rgb(71 85 105 / .45)}.dark .rate-tag b{color:#d3dbe7}.dark .provider-mark{background:#2b3446;color:#d1d9e5}.dark .provider-mark.openai{background:rgb(16 185 129 / .12);color:#99e0cb}.dark .provider-mark.anthropic{background:rgb(211 149 92 / .12);color:#e4b18b}.dark .provider-mark.gemini{background:rgb(129 140 248 / .12);color:#b4b9f4}.dark .provider-mark.domestic{background:rgb(96 165 250 / .12);color:#99b8f2}.dark .channel-metrics>div{background:rgb(15 23 42 / .4);border-color:rgb(71 85 105 / .25)}.dark .channel-metrics dd{color:#e8edf5}.dark .channel-metrics dd.text-state-healthy{color:#70ddba}.dark .channel-metrics dd.text-state-warning{color:#f3cf76}.dark .channel-metrics dd.text-state-critical{color:#f598a8}.dark .channel-metrics dd.text-state-unknown{color:#96a4b7}.dark .timeline-heading{border-color:rgb(71 85 105 / .35)}.dark .state-pill.state-healthy{background:rgb(16 185 129 / .12);color:#6ed6b4}.dark .state-pill.state-warning{background:rgb(245 158 11 / .13);color:#efd17e}.dark .state-pill.state-critical{background:rgb(244 63 94 / .12);color:#f29baa}.dark .state-pill.state-unknown{background:rgb(100 116 139 / .18);color:#a7b4c6}.dark .timeline-target.state-unknown>span{background:#617086}
@media (min-width:1536px) and (max-width:1740px){.sources-channel-card{padding:16px}.provider-mark{width:32px;height:36px}.channel-card-heading{gap:8px}.channel-identity h3{font-size:13px;min-height:39px}.state-pill{padding:4px;font-size:9px}.channel-metrics{gap:6px}.channel-metrics>div{padding:10px 7px}.channel-metrics dd{font-size:19px}}
@media (prefers-reduced-motion:reduce){.sources-channel-card,.timeline-target>span{transition:none}}
</style>
