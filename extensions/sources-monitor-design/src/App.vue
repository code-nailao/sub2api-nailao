<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import ProviderIcon from '@/components/user/monitor/ProviderIcon.vue'
import Icon from './Icon.vue'
import ChannelCard from './ChannelCard.vue'
import { brands, demoCards, realCards, labels, percent, seconds, time, type Brand, type Card, type Point, type Range, type Snapshot } from './model'

type Orders = { brands: Brand[]; cards: Record<string, string[]> }
const defaultOrders = (): Orders => ({ brands: brands.map(b => b.id), cards: {} })
const storageKey = 'sources-monitor-design-order-v1'
const orders = ref<Orders>(defaultOrders())
const dark = ref(true)
const collapsedSidebar = ref(false)
const snapshot = ref<Snapshot | null>(null)
const source = ref<'demo' | 'snapshot'>('demo')
const range = ref<Range>('90m')
const activeBrand = ref<Brand | 'all'>('all')
const query = ref('')
const onlyAttention = ref(false)
const expanded = ref<Record<string, boolean>>({})
const loading = ref(true)
const error = ref('')
const notice = ref('')
const sortDialog = ref<HTMLDialogElement>()
const detailDialog = ref<HTMLDialogElement>()
const helpDialog = ref<HTMLDialogElement>()
const draft = ref<Orders>(defaultOrders())
const sortBrand = ref<Brand | 'categories'>('categories')
const dragging = ref<string | null>(null)
const inspected = ref<{ card: Card; point: Point } | null>(null)
const ranges: Range[] = ['90m', '24h', '7d', '30d']
const rangeNames: Record<Range, string> = { '90m': '90分钟', '24h': '24小时', '7d': '7天', '30d': '30天' }
const cards = computed(() => source.value === 'demo' ? demoCards(snapshot.value, range.value) : realCards(snapshot.value))
const sortedBrands = computed(() => orders.value.brands.map(id => brands.find(b => b.id === id)!).filter(b => b && (b.id !== 'other' || cards.value.some(c => c.brand === 'other'))))
const counts = computed(() => Object.fromEntries(brands.map(b => [b.id, cards.value.filter(c => c.brand === b.id).length])))
const filteredCards = computed(() => cards.value.filter(c => (!query.value.trim() || `${c.fullName} ${c.subtitle}`.toLowerCase().includes(query.value.trim().toLowerCase())) && (!onlyAttention.value || ['warning', 'critical'].includes(c.state))))
function sortedCards(brand: Brand, ordering = orders.value) {
  const list = ordering.cards[`${source.value}:${brand}`] || []
  return cards.value.filter(c => c.brand === brand).sort((a, b) => {
    const ai = list.indexOf(a.id), bi = list.indexOf(b.id)
    return (ai < 0 ? 10000 : ai) - (bi < 0 ? 10000 : bi) || Number(/生图|\bimage\b/i.test(a.fullName)) - Number(/生图|\bimage\b/i.test(b.fullName))
  })
}
const sections = computed(() => sortedBrands.value.filter(b => activeBrand.value === 'all' || activeBrand.value === b.id).map(b => ({ ...b, cards: sortedCards(b.id).filter(c => filteredCards.value.includes(c)) })).filter(b => b.cards.length))
const totalVisible = computed(() => sections.value.reduce((n, b) => n + b.cards.length, 0))
function shownCards(list: Card[], id: Brand) { return expanded.value[id] || activeBrand.value !== 'all' || query.value || onlyAttention.value ? list : list.slice(0, 8) }
const draftRows = computed(() => sortBrand.value === 'categories'
  ? draft.value.brands.map(id => ({ id, label: brands.find(b => b.id === id)!.label, detail: `${counts.value[id] || 0} 个渠道`, provider: brands.find(b => b.id === id)!.provider }))
  : sortedCards(sortBrand.value, draft.value).map(c => ({ id: c.id, label: c.title, detail: c.subtitle, provider: c.provider })))

async function loadSnapshot() {
  loading.value = true
  try {
    const response = await fetch('/__preview/source', { cache: 'no-store' })
    if (!response.ok) throw new Error('主站快照尚未导入；当前使用设计演示数据。')
    snapshot.value = await response.json()
    error.value = ''
  } catch (e) { error.value = (e as Error).message }
  finally { loading.value = false }
}
function openSort(brand: Brand | 'categories' = 'categories') {
  draft.value = JSON.parse(JSON.stringify(orders.value))
  sortBrand.value = brand
  sortDialog.value?.showModal()
}
function setDraftRows(ids: string[]) {
  if (sortBrand.value === 'categories') draft.value.brands = ids as Brand[]
  else draft.value.cards[`${source.value}:${sortBrand.value}`] = ids
}
function move(id: string, delta: number) {
  const ids = draftRows.value.map(r => r.id), i = ids.indexOf(id), target = i + delta
  if (i < 0 || target < 0 || target >= ids.length) return
  ids.splice(target, 0, ids.splice(i, 1)[0])
  setDraftRows(ids)
}
function drop(target: string) {
  const ids = draftRows.value.map(r => r.id), from = ids.indexOf(dragging.value || ''), to = ids.indexOf(target)
  if (from >= 0 && to >= 0) { ids.splice(to, 0, ids.splice(from, 1)[0]); setDraftRows(ids) }
  dragging.value = null
}
function saveOrder() {
  const value = JSON.parse(JSON.stringify(draft.value))
  try { localStorage.setItem(storageKey, JSON.stringify(value)); orders.value = value; notice.value = '排列已保存到本机预览'; sortDialog.value?.close() }
  catch { notice.value = '浏览器未允许保存，当前排列仅在本次预览生效'; orders.value = value; sortDialog.value?.close() }
}
async function inspect(card: Card, point: Point) { inspected.value = { card, point }; await nextTick(); detailDialog.value?.showModal() }
function resetFilters() { query.value = ''; onlyAttention.value = false; activeBrand.value = 'all' }
onMounted(async () => {
  try {
    const saved = JSON.parse(localStorage.getItem(storageKey) || 'null')
    if (saved && Array.isArray(saved.brands) && saved.brands.length === brands.length && new Set(saved.brands).size === brands.length && saved.brands.every((id: string) => brands.some(b => b.id === id)) && saved.cards && Object.values(saved.cards).every(v => Array.isArray(v) && v.every(id => typeof id === 'string'))) orders.value = saved
  } catch { /* Discard corrupt preview preferences. */ }
  await loadSnapshot()
})
</script>

<template>
  <div class="app-shell" :class="{ light: !dark, 'sidebar-small': collapsedSidebar }">
    <aside class="sidebar">
      <a class="site-brand" href="/" aria-label="Sources 设计预览首页"><span class="sources-logo">S</span><span class="brand-type">Sources<span>CODE · AI WORKSPACE</span></span></a>
      <div class="workspace-caption">我的工作空间</div>
      <nav class="main-nav" aria-label="工作空间导航">
        <a href="https://main.example/dashboard" target="_blank" rel="noopener noreferrer"><Icon name="grid" /><span>仪表盘</span></a>
        <a href="https://main.example/keys" target="_blank" rel="noopener noreferrer"><Icon name="key" /><span>API 密钥</span></a>
        <a href="https://main.example/usage" target="_blank" rel="noopener noreferrer"><Icon name="chart" /><span>使用记录</span></a>
        <a href="https://main.example/available-channels" target="_blank" rel="noopener noreferrer"><Icon name="layers" /><span>可用渠道</span></a>
        <a href="#main" class="active" aria-current="page"><Icon name="activity" /><span>渠道状态</span><i></i></a>
        <a href="https://main.example/purchase" target="_blank" rel="noopener noreferrer"><Icon name="wallet" /><span>充值</span></a>
      </nav>
      <div class="sidebar-bottom"><div class="preview-note"><span class="tiny-label">DESIGN PREVIEW</span><strong>渠道状态 · 第一版</strong><p>让每条渠道的表现<br>一目了然。</p></div><button class="theme-toggle" @click="dark = !dark"><Icon :name="dark ? 'sun' : 'moon'" /><span>{{ dark ? '浅色模式' : '深色模式' }}</span></button><button class="theme-toggle collapse-button" @click="collapsedSidebar = !collapsedSidebar" :aria-label="collapsedSidebar ? '展开侧栏' : '收起侧栏'"><Icon name="chevron" :class="{ rotate: !collapsedSidebar }" /><span>收起侧栏</span></button></div>
    </aside>
    <div class="workspace">
      <header class="topbar"><div class="breadcrumb"><span>我的账户</span><Icon name="chevron" :size="13" /><strong>渠道状态</strong></div><div class="topbar-right"><span class="local-badge"><i></i>本地设计预览</span><a class="top-link" href="https://main.example/model-plaza" target="_blank" rel="noopener noreferrer"><Icon name="grid" :size="16" />模型广场<Icon name="external" :size="13" /></a><span class="avatar">S</span></div></header>
      <main id="main">
        <div class="page-heading"><div><div class="eyebrow">SOURCES / CHANNEL STATUS</div><h1><span class="heading-icon"><Icon name="chart" :size="23" /></span>渠道状态</h1><p>找到适合你的渠道，随时掌握可用性与响应表现。</p></div><div class="heading-actions"><button class="button quiet" @click="helpDialog?.showModal()"><Icon name="info" :size="16" />指标说明</button><button class="button" @click="openSort()"><Icon name="filter" :size="16" />自定义排序</button></div></div>

        <section class="control-panel" aria-label="监控筛选">
          <div class="control-top"><div class="range-switch" aria-label="统计时间范围"><button v-for="r in ranges" :key="r" :class="{ selected: source === 'demo' ? range === r : r === '7d' }" :aria-pressed="source === 'demo' ? range === r : r === '7d'" :disabled="source === 'snapshot'" @click="range = r">{{ r }}</button></div><div class="source-control"><label for="data-source">数据</label><select id="data-source" v-model="source"><option value="demo">设计演示</option><option value="snapshot" :disabled="!snapshot">主站只读快照</option></select><button class="icon-button" aria-label="重新读取本地快照" title="重新读取本地快照" :disabled="loading" @click="loadSnapshot"><Icon name="refresh" :class="{ spinning: loading }" :size="16" /></button></div></div>
          <div class="control-bottom"><div class="overview"><span><b>{{ cards.length }}</b> 个渠道</span></div><span class="snapshot-time">{{ source === 'demo' ? '演示指标 · 不代表实时状态' : '快照于 ' + time(snapshot!.captured_at, true) }}</span></div>
        </section>

        <div v-if="source === 'snapshot'" class="data-notice"><Icon name="info" :size="17" /><span>主站当前为 V1：可用率为近 7 天探测结果；缓存率与首 Token 暂无数据。历史条展示本次保存的探测记录。</span></div>
        <div v-else class="demo-notice"><span class="outlined-dot"></span>{{ snapshot ? '真实渠道名称与用户倍率，搭配演示指标预览新版布局。' : '当前使用演示渠道与指标预览新版布局。' }}</div>
        <div v-if="error" class="data-notice">{{ error }}</div>

        <div class="filter-row"><div class="brand-tabs" aria-label="渠道分类"><button :class="{ selected: activeBrand === 'all' }" :aria-pressed="activeBrand === 'all'" @click="activeBrand = 'all'"><Icon name="grid" :size="15" />全部<span>{{ cards.length }}</span></button><button v-for="brand in sortedBrands" :key="brand.id" :class="{ selected: activeBrand === brand.id }" :aria-pressed="activeBrand === brand.id" @click="activeBrand = brand.id"><Icon v-if="brand.id === 'other'" name="layers" :size="16" /><ProviderIcon v-else :provider="brand.provider" :size="16" />{{ brand.label }}<span>{{ counts[brand.id] }}</span></button></div><div class="search-wrap"><Icon name="search" :size="17" /><input v-model="query" aria-label="搜索渠道" placeholder="搜索渠道名称…" /><button v-if="query" class="clear-search" aria-label="清空搜索" @click="query = ''"><Icon name="close" :size="14" /></button></div></div>
        <div class="list-meta"><span>{{ totalVisible }} 个渠道 <span class="subtle">/ 按自定义顺序展示</span></span><label class="attention-switch"><input v-model="onlyAttention" type="checkbox" /><span class="switch-track"></span>只看需关注</label></div>

        <section v-for="section in sections" :key="section.id" class="brand-section" :aria-labelledby="`brand-${section.id}`">
          <div class="section-heading"><div class="section-title"><span v-if="section.id === 'domestic'" class="provider-stack"><span v-for="p in (['deepseek', 'kimi', 'zhipu'] as const)" :key="p" :class="p"><ProviderIcon :provider="p" :size="18" /></span></span><span v-else class="section-mark" :class="section.provider"><Icon v-if="section.id === 'other'" name="layers" :size="22" /><ProviderIcon v-else :provider="section.provider" :size="22" /></span><h2 :id="`brand-${section.id}`">{{ section.label }}</h2><span class="section-count">{{ section.cards.length }}</span><span class="section-caption">{{ section.caption }}</span></div><button class="section-sort" :aria-label="`调整${section.label}内部顺序`" @click="openSort(section.id)"><Icon name="filter" :size="14" /><span>调整顺序</span></button></div>
          <div class="cards-grid"><ChannelCard v-for="card in shownCards(section.cards, section.id)" :key="card.id" :card="card" :range-label="rangeNames[range]" @inspect="inspect" /></div>
          <button v-if="section.cards.length > 8 && activeBrand === 'all' && !query && !onlyAttention" class="expand-section" :aria-expanded="!!expanded[section.id]" @click="expanded[section.id] = !expanded[section.id]">{{ expanded[section.id] ? '收起部分渠道' : `展开其余 ${section.cards.length - 8} 个渠道` }}<Icon name="down" :size="14" :class="{ rotate: expanded[section.id] }" /></button>
        </section>
        <div v-if="!sections.length" class="empty-state"><Icon name="search" :size="32" /><h2>没有找到匹配的渠道</h2><p>试试其他名称，或清除筛选条件。</p><button class="button" @click="resetFilters">清除筛选</button></div>
        <footer class="page-footer"><div class="legend"><span v-for="(label, state) in labels" :key="state"><i class="dot" :class="state"></i>{{ label }}</span></div><span>Sources Code <span class="subtle">· 渠道状态设计预览</span></span></footer>
        <p v-if="notice" class="saved-message" role="status">{{ notice }}<button aria-label="关闭提示" @click="notice = ''"><Icon name="close" :size="15" /></button></p>
      </main>
    </div>

    <dialog ref="sortDialog" class="sort-dialog" aria-labelledby="sort-title" @close="dragging = null" @click="event => { if (event.target === sortDialog) sortDialog?.close() }">
      <header class="dialog-heading"><div><span class="eyebrow">MAKE IT YOURS</span><h2 id="sort-title">自定义展示顺序</h2><p>大分类和分类内的渠道，都按你的习惯排列。</p></div><button class="icon-button" aria-label="关闭排序" @click="sortDialog?.close()"><Icon name="close" /></button></header>
      <div class="sort-content"><nav class="sort-nav" aria-label="排序范围"><button :class="{ selected: sortBrand === 'categories' }" @click="sortBrand = 'categories'"><Icon name="layers" />大分类顺序</button><button v-for="brand in brands" :key="brand.id" :class="{ selected: sortBrand === brand.id }" @click="sortBrand = brand.id"><Icon v-if="brand.id === 'other'" name="layers" :size="18" /><ProviderIcon v-else :provider="brand.provider" :size="18" />{{ brand.label }}<span>{{ counts[brand.id] }}</span></button></nav><div class="sort-list-wrap"><p class="sort-tip">拖动调整，或使用右侧上下按钮。</p><ol class="sort-list"><li v-for="(row, i) in draftRows" :key="row.id" :draggable="true" :class="{ dragging: dragging === row.id }" @dragstart="event => { dragging = row.id; event.dataTransfer?.setData('text/plain', row.id) }" @dragend="dragging = null" @dragover.prevent @drop.prevent="drop(row.id)"><span class="drag-grip" aria-hidden="true">⠿</span><span class="sort-rank">{{ String(i + 1).padStart(2, '0') }}</span><ProviderIcon :provider="row.provider" :size="20" /><div class="sort-row-title"><strong>{{ row.label }}</strong><small>{{ row.detail }}</small></div><div class="move-buttons"><button class="icon-button" :disabled="i === 0" :aria-label="`上移${row.label}`" @click="move(row.id, -1)"><Icon name="down" class="rotate" :size="15" /></button><button class="icon-button" :disabled="i === draftRows.length - 1" :aria-label="`下移${row.label}`" @click="move(row.id, 1)"><Icon name="down" :size="15" /></button></div></li></ol><p v-if="!draftRows.length" class="sort-tip">这个分类暂时没有渠道。</p></div></div>
      <footer class="dialog-footer"><button class="text-button" @click="draft = defaultOrders()">恢复默认</button><span class="subtle">预览顺序保存在本机</span><button class="button" @click="sortDialog?.close()">取消</button><button class="button primary" @click="saveOrder"><Icon name="check" :size="16" />保存排列</button></footer>
    </dialog>

    <dialog ref="detailDialog" class="detail-dialog" aria-labelledby="detail-title"><template v-if="inspected"><header class="dialog-heading"><div><span class="eyebrow">{{ inspected.card.source === 'demo' ? '演示时段详情' : '主站探测快照' }}</span><h2 id="detail-title">{{ inspected.card.title }}</h2><p>{{ time(inspected.point.at, true) }} · {{ labels[inspected.point.state] }}</p></div><button class="icon-button" aria-label="关闭详情" @click="detailDialog?.close()"><Icon name="close" /></button></header><div class="detail-content"><dl class="detail-metrics"><div><dt>可用率</dt><dd>{{ percent(inspected.point.availability) }}</dd></div><div><dt>缓存率</dt><dd>{{ percent(inspected.point.cache) }}</dd></div><div><dt>首 Token P50</dt><dd>{{ seconds(inspected.point.ttft) }}</dd></div></dl><p v-if="inspected.card.source === 'snapshot'">该记录来自 V1 主动探测。缓存率和首 Token 未提供，保留为空；该渠道最近一次对话延迟为 {{ seconds(inspected.card.latency) }}。</p><p v-else>这里展示演示统计，用于确认详情布局。正式版本将使用 V2 对应时段的真实聚合数据。</p></div></template></dialog>

    <dialog ref="helpDialog" class="detail-dialog" aria-labelledby="help-title"><header class="dialog-heading"><div><span class="eyebrow">READING THE SIGNAL</span><h2 id="help-title">如何看懂渠道状态</h2></div><button class="icon-button" aria-label="关闭指标说明" @click="helpDialog?.close()"><Icon name="close" /></button></header><div class="detail-content help-content"><h3>用户倍率</h3><p>展示当前用户适用的分组倍率，最多保留三位小数。实际费用仍由模型价格、用量及适用计费规则决定。</p><h3>缓存率</h3><p>读缓存 Token 占可统计输入 Token 的比例。它受请求内容影响，低缓存率不直接代表渠道不可用。</p><h3>可用率与首 Token</h3><p>V2 使用所选时间范围的聚合数据。首 Token P50 表示一半请求在这个时间内收到首个 Token，数值越低通常响应越快。</p><h3>历史状态条</h3><p>V2 每条对应一个统计时段；主站 V1 快照每条对应一次真实探测。灰色表示未知或样本不足。点击色条可查看详情。</p><h3>这是一份设计预览</h3><p>演示模式的性能指标为示例。主站快照保持读取时的数据；切换时间或重读本地快照不会生成新请求，也不会触发模型检测。</p></div></dialog>
  </div>
</template>
