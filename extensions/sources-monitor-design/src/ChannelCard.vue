<template>
  <article class="channel-card" :data-state="card.state">
    <header class="card-header">
      <span class="provider-mark" :class="card.provider">
        <span v-if="card.brand === 'domestic' && /国模/.test(card.fullName)" class="mini-stack"><ProviderIcon provider="deepseek" :size="21" /><ProviderIcon provider="kimi" :size="13" /></span>
        <ProviderIcon v-else :provider="card.provider" :size="25" />
      </span>
      <div class="card-identity"><h3 :title="card.fullName">{{ card.title }}</h3><p>{{ card.subtitle }}</p></div>
      <span class="status-pill" :class="card.state"><i></i>{{ labels[card.state] }}</span>
    </header>
    <div class="card-tags"><span class="provider-tag" :class="card.provider">{{ card.brand === 'domestic' && /国模/.test(card.fullName) ? '多模型' : providerLabels[card.provider] }}</span><span class="rate-tag" title="优先使用当前用户的专属倍率；没有专属倍率时使用分组倍率。">用户倍率 <b>{{ rate(card.multiplier) }}</b></span></div>
    <dl class="metrics">
      <div><dt>缓存率 <span v-if="card.source === 'snapshot'" title="主站 V1 接口未提供缓存统计">ⓘ</span></dt><dd>{{ percent(card.cache) }}</dd></div>
      <div><dt>可用率<span v-if="card.source === 'snapshot'" class="metric-note"> · 7天</span></dt><dd :class="card.availability == null ? '' : card.availability >= 95 ? 'healthy-text' : card.availability >= 80 ? 'warning-text' : 'critical-text'">{{ percent(card.availability) }}</dd></div>
      <div><dt>首 Token <span class="metric-note">P50</span></dt><dd :class="card.ttft != null && card.ttft > 10000 ? 'warning-text' : ''">{{ seconds(card.ttft) }}</dd></div>
    </dl>
    <div class="timeline-top"><span>{{ card.source === 'demo' ? `近 ${rangeLabel} · ${card.points.length} 个时段` : `最近 ${card.points.length} 次探测` }}</span><span v-if="card.source === 'demo'" class="sample-label">演示</span><span v-else>只读快照</span></div>
    <div class="timeline" role="group" :aria-label="`${card.title}历史状态`" @mouseleave="hovered = null">
      <button v-for="(point, i) in card.points" :key="point.at" type="button" :class="['bar', point.state]" :aria-label="pointLabel(point)" @mouseenter="hovered = i" @focus="hovered = i" @blur="hovered = null" @click="emit('inspect', card, point)" @keydown.esc="hovered = null"><span></span></button>
      <div v-if="hovered !== null && card.points[hovered]" class="timeline-tooltip" role="tooltip"><strong>{{ time(card.points[hovered].at, true) }} <span :class="card.points[hovered].state + '-text'">{{ labels[card.points[hovered].state] }}</span></strong><p v-if="card.source === 'demo'">可用率 {{ percent(card.points[hovered].availability) }} · 首 Token {{ seconds(card.points[hovered].ttft) }}</p><p v-else>主站 V1 探测记录 · 点击查看</p></div>
    </div>
    <div class="timeline-axis"><span>{{ card.points.length ? time(card.points[0].at, rangeLabel !== '90分钟') : '过去' }}</span><span>{{ card.source === 'demo' ? '现在' : card.points.length ? time(card.points[card.points.length - 1].at) : '—' }}</span></div>
  </article>
</template>
<script setup lang="ts">
import { ref } from 'vue'
import ProviderIcon from '@/components/user/monitor/ProviderIcon.vue'
import { labels, percent, rate, seconds, time, providerLabels, type Card, type Point } from './model'
const props = defineProps<{ card: Card; rangeLabel: string }>()
const emit = defineEmits<{ inspect: [card: Card, point: Point] }>()
const hovered = ref<number | null>(null)
function pointLabel(point: Point) { return `${props.card.title} · ${time(point.at, true)} · ${labels[point.state]}${props.card.source === 'demo' ? ` · 演示可用率 ${percent(point.availability)}` : ''}` }
</script>
